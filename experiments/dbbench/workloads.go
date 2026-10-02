package main

// Application-shaped workload groups (suite v2): "append", an append-heavy
// event log, and "mutable", an issue board with hot rows. Unlike the core
// group, this code is written once against conn/backend and runs unchanged
// against RepoDB (either persistence mode) and external MySQL-compatible
// servers.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
)

// workloadGroups lists every -workloads value in execution order.
var workloadGroups = []string{"core", "append", "mutable"}

// parseWorkloads validates a comma-separated -workloads value and returns the
// groups in execution order.
func parseWorkloads(s string) ([]string, error) {
	want := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		known := false
		for _, g := range workloadGroups {
			known = known || part == g
		}
		if !known || want[part] {
			return nil, fmt.Errorf("invalid -workloads %q: want a comma-separated subset of %s", s, strings.Join(workloadGroups, ","))
		}
		want[part] = true
	}
	var groups []string
	for _, g := range workloadGroups {
		if want[g] {
			groups = append(groups, g)
		}
	}
	return groups, nil
}

func (c config) runs(group string) bool {
	for _, g := range c.Workloads {
		if g == group {
			return true
		}
	}
	return false
}

// growthPoint is one round of a group's growth phase.
type growthPoint struct {
	Group         string   `json:"group"`
	Rows          int      `json:"rows"`
	Round         int      `json:"round"`
	TableRows     int      `json:"table_rows"`
	WriteRequests int      `json:"write_requests"`
	WriteP50      float64  `json:"write_p50_ms"`
	WriteP95      float64  `json:"write_p95_ms"`
	WriteMax      float64  `json:"write_max_ms"`
	SyncMS        *float64 `json:"sync_ms,omitempty"`
	FixtureBytes  *int64   `json:"fixture_file_bytes,omitempty"`
	FixtureDelta  *int64   `json:"fixture_file_bytes_delta,omitempty"`
	JournalBefore *int64   `json:"journal_bytes_before_sync,omitempty"`
	JournalAfter  *int64   `json:"journal_bytes_after_sync,omitempty"`
	Heap          uint64   `json:"process_heap_bytes_at_end"`
}

// conn is one client session. Writes rejected by optimistic concurrency
// control return an error wrapping repository.ErrConflict, after the session
// has been made reusable.
type conn interface {
	exec(q string, args ...any) error
	query(q string, args ...any) ([][]string, error)
	tx(fn func(conn) error) error
	close() error
}

// backend is the persistence-specific part of a group: fixture setup,
// connections, and (RepoDB only) sync and peers.
type backend interface {
	open() error
	close() error
	connect() (conn, error)
	// root is the directory whose size measure() records.
	root() string
	syncs() bool
	sync() error
	housekeeping() error
	// journalBytes is the size of the working journal, if there is one.
	journalBytes() (int64, bool)
	// fixtureBytes is the local repository plus its remote.
	fixtureBytes() (int64, bool)
	// peer clones the published state into a new, open database.
	peer() (backend, error)
}

// RepoDB backend.

type repoBackend struct {
	dir, remote string
	d           *database
}

func newRepoBackend(parent, group string, rows int, options engine.Options) *repoBackend {
	dir := filepath.Join(parent, fmt.Sprintf("%s-%d", group, rows))
	return &repoBackend{dir: dir, remote: filepath.Join(dir, "remote.git"), d: &database{path: filepath.Join(dir, "local"), options: options}}
}

func (b *repoBackend) open() error {
	if err := os.Mkdir(b.dir, 0755); err != nil {
		return err
	}
	if err := git(b.dir, "init", "--bare", "--quiet", b.remote); err != nil {
		return err
	}
	if err := configureFixture(b.remote); err != nil {
		return err
	}
	if err := newRepo(b.d.path, b.remote); err != nil {
		return err
	}
	if _, err := integration.Enable(ctx, b.d.path, "origin"); err != nil {
		return err
	}
	return b.d.open()
}

func (b *repoBackend) close() error {
	if b.d.e == nil {
		return nil
	}
	return b.d.e.Close()
}

func (b *repoBackend) connect() (conn, error) {
	s, err := b.d.e.NewSession()
	if err != nil {
		return nil, err
	}
	return rdbConn{s}, nil
}

func (b *repoBackend) root() string        { return b.dir }
func (b *repoBackend) syncs() bool         { return true }
func (b *repoBackend) sync() error         { return b.d.sync() }
func (b *repoBackend) housekeeping() error { return gitGC(b.d.path, b.remote) }

func (b *repoBackend) journalBytes() (int64, bool) {
	w := b.d.e.WorkingState()
	if w == nil {
		return 0, false
	}
	n, err := directoryBytes(w.Dir())
	return n, err == nil
}

func (b *repoBackend) fixtureBytes() (int64, bool) {
	var total int64
	for _, p := range []string{b.d.path, b.remote} {
		n, err := directoryBytes(p)
		if err != nil {
			return 0, false
		}
		total += n
	}
	return total, true
}

func (b *repoBackend) peer() (backend, error) {
	p := &repoBackend{dir: b.dir, remote: b.remote, d: &database{path: filepath.Join(b.dir, "peer"), options: b.d.options}}
	if err := newRepo(p.d.path, b.remote); err != nil {
		return nil, err
	}
	if _, err := integration.Enable(ctx, p.d.path, "origin"); err != nil {
		return nil, err
	}
	return p, p.d.open()
}

type rdbConn struct{ s *engine.Session }

// rollbackConflict leaves a session whose write was rejected reusable.
func (c rdbConn) rollbackConflict(err error) error {
	if errors.Is(err, repository.ErrConflict) {
		if cleanup := c.s.Exec(ctx, "ROLLBACK"); cleanup != nil {
			return fmt.Errorf("rollback rejected write: %v (original: %v)", cleanup, err)
		}
	}
	return err
}

func (c rdbConn) exec(q string, args ...any) error {
	return c.rollbackConflict(c.s.Exec(ctx, q, args...))
}

func (c rdbConn) query(q string, args ...any) ([][]string, error) {
	r, err := c.s.Query(ctx, q, args...)
	if err != nil {
		return nil, c.rollbackConflict(err)
	}
	return stringRows(r.Rows), nil
}

func (c rdbConn) tx(fn func(conn) error) error {
	t, err := c.s.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(rdbTx{t}); err != nil {
		_ = t.Rollback(ctx)
		return err
	}
	return c.rollbackConflict(t.Commit(ctx))
}

func (c rdbConn) close() error { return c.s.Close() }

type rdbTx struct{ t *engine.Tx }

func (t rdbTx) exec(q string, args ...any) error { return t.t.Exec(ctx, q, args...) }
func (t rdbTx) query(q string, args ...any) ([][]string, error) {
	r, err := t.t.Query(ctx, q, args...)
	return stringRows(r.Rows), err
}
func (rdbTx) tx(func(conn) error) error { return errors.New("nested transaction") }
func (rdbTx) close() error              { return nil }

func stringRows(rows [][]any) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = make([]string, len(row))
		for j, v := range row {
			if v == nil {
				out[i][j] = "NULL"
			} else {
				out[i][j] = fmt.Sprint(v)
			}
		}
	}
	return out
}

// External backend.

type externalBackend struct {
	e   *externalDB
	dir string
}

func (b *externalBackend) open() error         { return b.e.open() }
func (b *externalBackend) close() error        { return b.e.close() }
func (b *externalBackend) root() string        { return b.dir }
func (b *externalBackend) syncs() bool         { return false }
func (b *externalBackend) sync() error         { return errors.New("external servers do not sync") }
func (b *externalBackend) housekeeping() error { return nil }
func (b *externalBackend) peer() (backend, error) {
	return nil, errors.New("external servers have no peers")
}
func (b *externalBackend) journalBytes() (int64, bool) {
	return 0, false
}
func (b *externalBackend) fixtureBytes() (int64, bool) { return 0, false }

func (b *externalBackend) connect() (conn, error) {
	c, err := b.e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return extConn{c}, nil
}

// externalConflict reports deadlocks and serialization failures (Dolt uses
// 1213 for both) as conflicts, so contended workloads count them as rejected
// attempts the same way RepoDB's are.
func externalConflict(err error) error {
	var m *mysql.MySQLError
	if errors.As(err, &m) && (m.Number == 1213 || m.Number == 1205) {
		return fmt.Errorf("%w: %v", repository.ErrConflict, err)
	}
	return err
}

type extConn struct{ c *sql.Conn }

func (c extConn) exec(q string, args ...any) error {
	_, err := c.c.ExecContext(ctx, q, args...)
	return externalConflict(err)
}

func (c extConn) query(q string, args ...any) ([][]string, error) {
	rows, err := c.c.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, externalConflict(err)
	}
	return scanRows(rows)
}

func (c extConn) tx(fn func(conn) error) error {
	t, err := c.c.BeginTx(ctx, nil)
	if err != nil {
		return externalConflict(err)
	}
	if err := fn(extTx{t}); err != nil {
		_ = t.Rollback()
		return err
	}
	return externalConflict(t.Commit())
}

func (c extConn) close() error { return c.c.Close() }

type extTx struct{ t *sql.Tx }

func (t extTx) exec(q string, args ...any) error {
	_, err := t.t.ExecContext(ctx, q, args...)
	return externalConflict(err)
}
func (t extTx) query(q string, args ...any) ([][]string, error) {
	rows, err := t.t.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, externalConflict(err)
	}
	return scanRows(rows)
}
func (extTx) tx(func(conn) error) error { return errors.New("nested transaction") }
func (extTx) close() error              { return nil }

func scanRows(rows *sql.Rows) ([][]string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		targets := make([]any, len(cols))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range values {
			row[i] = "NULL"
			if v.Valid {
				row[i] = v.String
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Deterministic fixture data.

// mix is SplitMix64's finalizer: a fixed, well-distributed hash of x.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

var fillerWords = strings.Fields(`agent tool call result error retry timeout cache index
query table commit branch merge sync remote local snapshot journal row column value
issue board status review done open triage assignee priority comment label epic task
bug feature parse render build test deploy config token request response stream event
session step trace span latency budget plan draft summary context memory file path`)

// filler is n bytes of pseudo-prose: realistic for compression, unlike hex.
func filler(seed uint64, n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	for i := uint64(0); b.Len() < n; i++ {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fillerWords[mix(seed*1_000_003+i)%uint64(len(fillerWords))])
	}
	return b.String()[:n]
}

const (
	eventsPerSession = 100
	eventKinds       = 8
	eventEpoch       = int64(1_767_225_600_000) // 2026-01-01T00:00:00Z in ms
)

var kindNames = [eventKinds]string{"user_message", "model_response", "tool_call", "tool_result", "error", "retry", "checkpoint", "summary"}

type event struct {
	id, ts, session int64
	kind, payload   string
}

// eventTS is strictly increasing in id: the jitter (< 10 ms) is smaller than
// the 10 ms step.
func eventTS(id int64) int64 { return eventEpoch + id*10 + int64(mix(uint64(id))%10) }

// makeEvent is event id. Sessions are contiguous runs of eventsPerSession
// events, as a trace's events are.
func makeEvent(id int64) event {
	session := (id-1)/eventsPerSession + 1
	kind := kindNames[id%eventKinds]
	head := fmt.Sprintf(`{"session":%d,"step":%d,"kind":"%s","detail":"`, session, (id-1)%eventsPerSession, kind)
	return event{id: id, ts: eventTS(id), session: session, kind: kind, payload: head + filler(uint64(id), 256-len(head)-2) + `"}`}
}

var issueStatuses = []string{"open", "triage", "doing", "review", "done"}

const issueAssignees = 20

type issue struct {
	id          int64
	status      string
	assignee    *string
	priority    int64
	title, body string
	updatedAt   int64
}

func assigneeName(n uint64) string { return fmt.Sprintf("agent-%02d", n) }

// makeIssue is issue id. About one in eleven issues is unassigned.
func makeIssue(id int64) issue {
	h := mix(uint64(id))
	is := issue{id: id, status: issueStatuses[h%uint64(len(issueStatuses))], priority: int64(h>>8) % 4, updatedAt: eventEpoch + id}
	if a := (h >> 16) % (issueAssignees + 2); a < issueAssignees {
		name := assigneeName(a)
		is.assignee = &name
	}
	is.title = fmt.Sprintf("Issue %d: %s", id, filler(h, 60))
	is.body = filler(h^0xaaaa, 1024)
	return is
}

func commentBody(id int64) string { return filler(mix(uint64(id))^0x5555, 200) }

// hotPicker draws issue ids with Zipf skew toward the newest issues: on a
// board the recently filed issues are the hot ones.
type hotPicker struct {
	z *rand.Zipf
	n int64
}

func newHotPicker(seed int64, n int64) hotPicker {
	return hotPicker{z: rand.NewZipf(rand.New(rand.NewSource(seed)), 1.1, 1, uint64(n-1)), n: n}
}

func (h hotPicker) next() int64 { return h.n - int64(h.z.Uint64()) }

// Group execution.

// env runs one group at one size against one backend.
type env struct {
	r     *report
	group string
	rows  int
	b     backend
}

// measure records one workload, tagged with the group. Workload names are
// unique across groups; setup steps shared by both groups use setup().
func (e env) measure(name string, clients, n int, allowConflict bool, fn func(int, int) error) error {
	err := e.r.measure(e.b.root(), e.rows, name, clients, n, allowConflict, fn)
	e.r.Results[len(e.r.Results)-1].Group = e.group
	return err
}

func (e env) once(name string, fn func() error) error {
	return e.measure(name, 1, 1, false, func(int, int) error { return fn() })
}

// setup records a one-off step both groups share, such as bulk_load, as
// <group>_<step>.
func (e env) setup(step string, fn func() error) error { return e.once(e.group+"_"+step, fn) }

// runWorkloadGroups runs the append and mutable groups selected by
// -workloads at every -workload-rows size. open returns a fresh, unopened
// backend for one group and size.
func runWorkloadGroups(r *report, open func(group string, rows int) backend) error {
	var failures []error
	for _, group := range []string{"append", "mutable"} {
		if !r.Config.runs(group) {
			continue
		}
		for _, rows := range r.Config.WorkloadRows {
			b := open(group, rows)
			e := env{r: r, group: group, rows: rows, b: b}
			err := e.setup("open", b.open)
			if err == nil {
				if group == "append" {
					err = runAppend(e)
				} else {
					err = runMutable(e)
				}
			}
			if closeErr := b.close(); err == nil {
				err = closeErr
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("%s rows=%d: %w", group, rows, err))
			}
		}
	}
	return errors.Join(failures...)
}

// loadAndPublish creates the schema, bulk-loads in 500-row statements inside
// one transaction (as the core group does), verifies the count and publishes.
func loadAndPublish(e env, c conn, schema []string, load func(tx conn) error, count string, rows int) error {
	if err := e.setup("schema", func() error {
		for _, q := range schema {
			if err := c.exec(q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := e.setup("bulk_load", func() error { return c.tx(load) }); err != nil {
		return err
	}
	if err := expectRows(c, count, strconv.Itoa(rows)); err != nil {
		return err
	}
	if e.b.syncs() {
		if err := e.setup("initial_publish_sync", e.b.sync); err != nil {
			return err
		}
	}
	return e.b.housekeeping()
}

// insertBatches runs INSERT INTO table VALUES … for ids from..to in 500-row
// statements; row renders one id's parenthesised tuple.
func insertBatches(tx conn, table string, from, to int64, row func(int64) string) error {
	for start := from; start <= to; start += 500 {
		var q strings.Builder
		q.WriteString("INSERT INTO " + table + " VALUES ")
		for id := start; id <= min(to, start+499); id++ {
			if id > start {
				q.WriteByte(',')
			}
			q.WriteString(row(id))
		}
		if err := tx.exec(q.String()); err != nil {
			return err
		}
	}
	return nil
}

func expectRows(c conn, q, value string) error {
	rows, err := c.query(q)
	if err != nil {
		return err
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != value {
		return fmt.Errorf("%s: expected %q, got %v", q, value, rows)
	}
	return nil
}

// concurrently runs a workload once per -clients level with one connection
// per client. fn receives the client level, worker and request index.
func concurrently(e env, name string, fn func(conns []conn, clients, w, i int) error) error {
	for _, clients := range e.r.Config.Clients {
		conns := make([]conn, clients)
		for i := range conns {
			c, err := e.b.connect()
			if err != nil {
				return err
			}
			conns[i] = c
			defer c.close()
		}
		if err := e.measure(name, clients, e.r.Config.Requests, true, func(w, i int) error { return fn(conns, clients, w, i) }); err != nil {
			return err
		}
	}
	return nil
}

// growth runs rounds of burst then sync, recording one growthPoint per
// round, then times a fresh peer's clone and pull of the whole history and
// compares checksum with the peer.
func growth(e env, c conn, burst func(round int) ([]float64, error), tableRows func() int, checksum []string) error {
	if err := e.b.housekeeping(); err != nil {
		return err
	}
	prev, _ := e.b.fixtureBytes()
	for round := 1; round <= e.r.Config.GrowthRounds; round++ {
		fmt.Fprintf(os.Stderr, "%s_growth rows=%d round=%d\n", e.group, e.rows, round)
		var durations []float64
		var err error
		pprof.Do(ctx, pprof.Labels("workload", e.group+"_growth_burst"), func(context.Context) { durations, err = burst(round) })
		if err != nil {
			return fmt.Errorf("growth round %d: %w", round, err)
		}
		sort.Float64s(durations)
		p := growthPoint{Group: e.group, Rows: e.rows, Round: round, TableRows: tableRows(), WriteRequests: len(durations),
			WriteP50: percentile(durations, .5), WriteP95: percentile(durations, .95), WriteMax: percentile(durations, 1)}
		if e.b.syncs() {
			if n, ok := e.b.journalBytes(); ok {
				p.JournalBefore = &n
			}
			start := time.Now()
			pprof.Do(ctx, pprof.Labels("workload", e.group+"_growth_sync"), func(context.Context) { err = e.b.sync() })
			if err != nil {
				return fmt.Errorf("growth round %d sync: %w", round, err)
			}
			ms := float64(time.Since(start)) / float64(time.Millisecond)
			p.SyncMS = &ms
			if n, ok := e.b.journalBytes(); ok {
				p.JournalAfter = &n
			}
		}
		if n, ok := e.b.fixtureBytes(); ok {
			delta := n - prev
			p.FixtureBytes, p.FixtureDelta, prev = &n, &delta, n
		}
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		p.Heap = mem.HeapAlloc
		e.r.Series = append(e.r.Series, p)
	}
	if !e.b.syncs() {
		return nil
	}
	var peer backend
	if err := e.setup("peer_pull_after_growth", func() error {
		var err error
		peer, err = e.b.peer()
		return err
	}); err != nil {
		return err
	}
	defer peer.close()
	pc, err := peer.connect()
	if err != nil {
		return err
	}
	defer pc.close()
	for _, q := range checksum {
		want, err := c.query(q)
		if err != nil {
			return err
		}
		got, err := pc.query(q)
		if err != nil {
			return err
		}
		if fmt.Sprint(want) != fmt.Sprint(got) {
			err := fmt.Errorf("peer convergence: %s: local %v, peer %v", q, want, got)
			e.r.Results[len(e.r.Results)-1].VerificationError = err.Error()
			return err
		}
	}
	return nil
}

func timed(durations *[]float64, fn func() error) error {
	start := time.Now()
	err := fn()
	*durations = append(*durations, float64(time.Since(start))/float64(time.Millisecond))
	return err
}

// The append group: an agent trace / event log.

const eventsInsert = "INSERT INTO events VALUES (?, ?, ?, ?, ?)"

func insertEvent(c conn, id int64) error {
	ev := makeEvent(id)
	return c.exec(eventsInsert, ev.id, ev.ts, ev.session, ev.kind, ev.payload)
}

func runAppend(e env) error {
	c, err := e.b.connect()
	if err != nil {
		return err
	}
	defer c.close()
	n := int64(e.rows)
	if err := loadAndPublish(e, c, []string{
		"CREATE TABLE events (id BIGINT PRIMARY KEY, ts BIGINT NOT NULL, session_id BIGINT NOT NULL, kind VARCHAR(32) NOT NULL, payload TEXT NOT NULL, INDEX by_session (session_id, ts), INDEX by_ts (ts))",
	}, func(tx conn) error {
		return insertBatches(tx, "events", 1, n, func(id int64) string {
			ev := makeEvent(id)
			return fmt.Sprintf("(%d,%d,%d,'%s','%s')", ev.id, ev.ts, ev.session, ev.kind, ev.payload)
		})
	}, "SELECT COUNT(*) FROM events", e.rows); err != nil {
		return err
	}

	// Reads against the published preload.
	requests := e.r.Config.Requests
	rng := rand.New(rand.NewSource(1))
	recentFrom := eventTS(max(1, n-999))
	if err := e.measure("events_recent", 1, requests, false, func(int, int) error {
		rows, err := c.query("SELECT id, ts, kind FROM events WHERE ts >= ? ORDER BY ts DESC LIMIT 100", recentFrom)
		if err == nil && (len(rows) != 100 || rows[0][0] != strconv.FormatInt(n, 10)) {
			return fmt.Errorf("events_recent: expected 100 rows from id %d, got %d", n, len(rows))
		}
		return err
	}); err != nil {
		return err
	}
	sessions := n / eventsPerSession
	if err := e.measure("events_session", 1, requests, false, func(int, int) error {
		rows, err := c.query("SELECT id, ts, kind FROM events WHERE session_id = ? ORDER BY ts", 1+rng.Int63n(sessions))
		if err == nil && len(rows) != eventsPerSession {
			return fmt.Errorf("events_session: expected %d rows, got %d", eventsPerSession, len(rows))
		}
		return err
	}); err != nil {
		return err
	}
	tailFrom := eventTS(n - n/10 + 1)
	if err := e.measure("events_kind_count", 1, requests, false, func(int, int) error {
		rows, err := c.query("SELECT kind, COUNT(*) FROM events WHERE ts >= ? GROUP BY kind", tailFrom)
		if err == nil && len(rows) != eventKinds {
			return fmt.Errorf("events_kind_count: expected %d kinds, got %d", eventKinds, len(rows))
		}
		return err
	}); err != nil {
		return err
	}

	// Writes. Ids are claimed from one counter; inserted counts acknowledged
	// inserts, newest the highest acknowledged id.
	var next, inserted, newest atomic.Int64
	next.Store(n)
	acknowledge := func(id, count int64) {
		inserted.Add(count)
		for cur := newest.Load(); id > cur && !newest.CompareAndSwap(cur, id); cur = newest.Load() {
		}
	}
	newest.Store(n)
	if err := e.measure("append_single", 1, requests, false, func(int, int) error {
		id := next.Add(1)
		err := insertEvent(c, id)
		if err == nil {
			acknowledge(id, 1)
		}
		return err
	}); err != nil {
		return err
	}
	if err := e.measure("append_batch_100", 1, requests, false, func(int, int) error {
		first := next.Add(100) - 99
		err := c.tx(func(tx conn) error {
			for id := first; id < first+100; id++ {
				if err := insertEvent(tx, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			acknowledge(first+99, 100)
		}
		return err
	}); err != nil {
		return err
	}
	if err := e.measure("event_update_rare", 1, requests, false, func(_ int, i int) error {
		return c.exec("UPDATE events SET payload = ? WHERE id = ?", fmt.Sprintf(`{"redacted":%d}`, i), 1+rng.Int63n(n))
	}); err != nil {
		return err
	}
	if err := concurrently(e, "append_concurrent", func(conns []conn, _, w, _ int) error {
		id := next.Add(1)
		err := insertEvent(conns[w], id)
		if err == nil {
			acknowledge(id, 1)
		}
		return err
	}); err != nil {
		return err
	}
	if err := expectRows(c, "SELECT COUNT(*) FROM events", strconv.FormatInt(n+inserted.Load(), 10)); err != nil {
		return err
	}
	if err := expectRows(c, "SELECT id FROM events ORDER BY ts DESC LIMIT 1", strconv.FormatInt(newest.Load(), 10)); err != nil {
		return err
	}

	// Growth: 1,000 events in 100-event transactions, with one rare update per
	// 100 appends, then sync.
	return growth(e, c, func(round int) ([]float64, error) {
		var durations []float64
		for batch := 0; batch < 10; batch++ {
			first := next.Add(100) - 99
			if err := timed(&durations, func() error {
				return c.tx(func(tx conn) error {
					for id := first; id < first+100; id++ {
						if err := insertEvent(tx, id); err != nil {
							return err
						}
					}
					return nil
				})
			}); err != nil {
				return nil, err
			}
			inserted.Add(100)
			if err := timed(&durations, func() error {
				return c.exec("UPDATE events SET payload = ? WHERE id = ?", fmt.Sprintf(`{"redacted":"growth-%d-%d"}`, round, batch), 1+rng.Int63n(first))
			}); err != nil {
				return nil, err
			}
		}
		return durations, nil
	}, func() int { return int(n + inserted.Load()) }, []string{
		"SELECT COUNT(*), MAX(id), SUM(ts), SUM(LENGTH(payload)) FROM events",
	})
}

// The mutable group: an issue board with hot rows.

func runMutable(e env) error {
	c, err := e.b.connect()
	if err != nil {
		return err
	}
	defer c.close()
	n := int64(e.rows)
	if err := loadAndPublish(e, c, []string{
		"CREATE TABLE issues (id BIGINT PRIMARY KEY, status VARCHAR(16) NOT NULL, assignee VARCHAR(32), priority INT NOT NULL, title VARCHAR(120) NOT NULL, body TEXT NOT NULL, updated_at BIGINT NOT NULL, INDEX by_status (status, priority), INDEX by_assignee (assignee, status))",
		"CREATE TABLE comments (id BIGINT PRIMARY KEY, issue_id BIGINT NOT NULL, body TEXT NOT NULL, INDEX by_issue (issue_id))",
	}, func(tx conn) error {
		if err := insertBatches(tx, "issues", 1, n, func(id int64) string {
			is := makeIssue(id)
			assignee := "NULL"
			if is.assignee != nil {
				assignee = "'" + *is.assignee + "'"
			}
			return fmt.Sprintf("(%d,'%s',%s,%d,'%s','%s',%d)", is.id, is.status, assignee, is.priority, is.title, is.body, is.updatedAt)
		}); err != nil {
			return err
		}
		return insertBatches(tx, "comments", 1, 2*n, func(id int64) string {
			return fmt.Sprintf("(%d,%d,'%s')", id, (id-1)/2+1, commentBody(id))
		})
	}, "SELECT COUNT(*) FROM issues", e.rows); err != nil {
		return err
	}

	requests := e.r.Config.Requests
	rng := rand.New(rand.NewSource(2))
	hot := newHotPicker(3, n)
	var clock, nextComment, comments atomic.Int64
	clock.Store(eventEpoch + n)
	nextComment.Store(2 * n)
	comments.Store(2 * n)

	pointRead := func(c conn, id int64) error {
		rows, err := c.query("SELECT id, status, assignee, priority, title, body, updated_at FROM issues WHERE id = ?", id)
		if err == nil && len(rows) != 1 {
			return fmt.Errorf("issue %d: expected 1 row, got %d", id, len(rows))
		}
		return err
	}
	boardQuery := func(c conn, status string) error {
		rows, err := c.query("SELECT id, title, assignee, priority FROM issues WHERE status = ? ORDER BY priority LIMIT 50", status)
		if err == nil && len(rows) != 50 {
			return fmt.Errorf("board %s: expected 50 rows, got %d", status, len(rows))
		}
		return err
	}
	assigneeQuery := func(c conn, assignee string) error {
		rows, err := c.query("SELECT id, title, status FROM issues WHERE assignee = ? AND status <> 'done'", assignee)
		if err == nil && len(rows) == 0 {
			return fmt.Errorf("assignee %s: no open issues", assignee)
		}
		return err
	}
	updateStatus := func(c conn, id int64, status string) error {
		return c.exec("UPDATE issues SET status = ?, updated_at = ? WHERE id = ?", status, clock.Add(1), id)
	}
	addComment := func(c conn, issueID int64) error {
		id := nextComment.Add(1)
		err := c.tx(func(tx conn) error {
			if err := tx.exec("INSERT INTO comments VALUES (?, ?, ?)", id, issueID, commentBody(id)); err != nil {
				return err
			}
			return tx.exec("UPDATE issues SET updated_at = ? WHERE id = ?", clock.Add(1), issueID)
		})
		if err == nil {
			comments.Add(1)
		}
		return err
	}

	if err := e.measure("issue_point_read", 1, requests, false, func(int, int) error { return pointRead(c, hot.next()) }); err != nil {
		return err
	}
	if err := e.measure("issue_board_query", 1, requests, false, func(_ int, i int) error {
		return boardQuery(c, issueStatuses[i%len(issueStatuses)])
	}); err != nil {
		return err
	}
	if err := e.measure("issue_assignee_query", 1, requests, false, func(_ int, i int) error {
		return assigneeQuery(c, assigneeName(uint64(i%issueAssignees)))
	}); err != nil {
		return err
	}
	if err := e.measure("issue_update_status", 1, requests, false, func(int, int) error {
		return updateStatus(c, hot.next(), issueStatuses[rng.Intn(len(issueStatuses))])
	}); err != nil {
		return err
	}
	// lastTitle records each edited issue's final title for verification.
	lastTitle := map[int64]string{}
	if err := e.measure("issue_update_edit", 1, requests, false, func(_ int, i int) error {
		id := hot.next()
		title := fmt.Sprintf("Issue %d (edit %d): %s", id, i, filler(uint64(i), 40))
		err := c.tx(func(tx conn) error {
			return tx.exec("UPDATE issues SET title = ?, body = ?, updated_at = ? WHERE id = ?", title, filler(uint64(id)^uint64(i)<<32, 1024), clock.Add(1), id)
		})
		if err == nil {
			lastTitle[id] = title
		}
		return err
	}); err != nil {
		return err
	}
	if err := e.measure("issue_comment", 1, requests, false, func(int, int) error { return addComment(c, hot.next()) }); err != nil {
		return err
	}
	// 30% writes (two status updates, one comment), 70% reads.
	if err := concurrently(e, "issue_mixed_concurrent", func(conns []conn, clients, w, i int) error {
		r := rand.New(rand.NewSource(int64(clients*1000 + w*100 + i)))
		id := n - int64(rand.NewZipf(r, 1.1, 1, uint64(n-1)).Uint64())
		switch i % 10 {
		case 0, 1:
			return updateStatus(conns[w], id, issueStatuses[r.Intn(len(issueStatuses))])
		case 2:
			return addComment(conns[w], id)
		case 3, 4, 5, 6:
			return pointRead(conns[w], id)
		case 7, 8:
			return boardQuery(conns[w], issueStatuses[r.Intn(len(issueStatuses))])
		default:
			return assigneeQuery(conns[w], assigneeName(uint64(r.Intn(issueAssignees))))
		}
	}); err != nil {
		return err
	}
	if err := expectRows(c, "SELECT COUNT(*) FROM comments", strconv.FormatInt(comments.Load(), 10)); err != nil {
		return err
	}
	if hottest, ok := mostEdited(lastTitle); ok {
		if err := expectRows(c, fmt.Sprintf("SELECT title FROM issues WHERE id = %d", hottest), lastTitle[hottest]); err != nil {
			return err
		}
	}

	// Growth: 500 hot status updates and 100 comments, then sync.
	return growth(e, c, func(int) ([]float64, error) {
		var durations []float64
		for i := 0; i < 600; i++ {
			if err := timed(&durations, func() error {
				if i%6 == 5 {
					return addComment(c, hot.next())
				}
				return updateStatus(c, hot.next(), issueStatuses[rng.Intn(len(issueStatuses))])
			}); err != nil {
				return nil, err
			}
		}
		return durations, nil
	}, func() int { return e.rows }, []string{
		"SELECT COUNT(*), SUM(updated_at), SUM(priority), SUM(LENGTH(body)) FROM issues",
		"SELECT status, COUNT(*) FROM issues GROUP BY status ORDER BY status",
		"SELECT COUNT(*), MAX(id), SUM(issue_id) FROM comments",
	})
}

// mostEdited returns the highest edited id, the hottest under hotPicker.
func mostEdited(titles map[int64]string) (int64, bool) {
	var best int64
	for id := range titles {
		best = max(best, id)
	}
	return best, best > 0
}
