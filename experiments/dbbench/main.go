// dbbench is the application-level scorecard. Workloads never select a storage mode.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/nicbet/repodb/client"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
	"github.com/nicbet/repodb/server"
	"golang.org/x/sys/unix"
)

type config struct {
	Mode     string `json:"mode"`
	Rows     []int  `json:"rows"`
	Requests int    `json:"requests_per_client"`
	Clients  []int  `json:"clients"`
}

type measurement struct {
	Rows              int       `json:"rows"`
	Name              string    `json:"name"`
	Clients           int       `json:"clients"`
	Success           int       `json:"success"`
	Conflicts         int       `json:"conflicts"`
	Errors            []string  `json:"errors,omitempty"`
	VerificationError string    `json:"verification_error,omitempty"`
	WallMS            float64   `json:"wall_ms"`
	Ops               float64   `json:"successful_ops_per_second"`
	P50               float64   `json:"p50_ms"`
	P95               float64   `json:"p95_ms"`
	P99               float64   `json:"p99_ms"`
	Max               float64   `json:"max_ms"`
	Allocated         uint64    `json:"process_allocated_bytes"`
	Heap              uint64    `json:"process_heap_bytes_at_end"`
	Growth            int64     `json:"fixture_file_bytes_delta"`
	Durations         []float64 `json:"successful_request_ms"`
	Rejected          []float64 `json:"conflict_request_ms,omitempty"`
}

type report struct {
	Version     int           `json:"suite_version"`
	Started     time.Time     `json:"started"`
	Config      config        `json:"config"`
	Go          string        `json:"go"`
	Platform    string        `json:"platform"`
	Git         string        `json:"git"`
	Revision    string        `json:"revision"`
	WorkingTree string        `json:"working_tree_status"`
	Root        string        `json:"fixture_root"`
	Results     []measurement `json:"results"`
	PeakRSS     int64         `json:"process_peak_rss_bytes"`
	Failure     string        `json:"failure,omitempty"`
}

var ctx = context.Background()

func main() {
	mode := flag.String("mode", "native-git", "persistence: native-git, journal, or external")
	dsn := flag.String("dsn", "", "MySQL DSN for external mode (e.g. root@tcp(127.0.0.1:3306)/)")
	rows := flag.String("rows", "1000,10000,50000", "fixture sizes")
	clients := flag.String("clients", "1,4,16", "concurrent clients")
	requests := flag.Int("requests", 30, "requests per client per repeated workload")
	output := flag.String("output", "dbbench.json", "complete JSON report")
	parent := flag.String("temp-dir", "", "parent directory for fresh fixtures (choose benchmark filesystem)")
	verify := flag.String("verify-repo", "", "internal: fresh-process reopen check")
	verifyRows := flag.Int("verify-rows", 0, "internal: expected recovered row count")
	verifyValue := flag.String("verify-value", "", "internal: expected recovered last write")
	flag.Parse()
	if *verify != "" {
		e, err := engine.OpenWithOptions(ctx, *verify, engine.Options{Persistence: engine.PersistenceMode(*mode)})
		if err == nil {
			var s *engine.Session
			s, err = e.NewSession()
			if err == nil {
				err = expect(s, "SELECT value FROM marker WHERE id = 1", "durable")
				if err == nil {
					err = expect(s, "SELECT COUNT(*) FROM bench", strconv.Itoa(*verifyRows))
				}
				if err == nil {
					err = expect(s, "SELECT value FROM bench WHERE id = 1", *verifyValue)
				}
				_ = s.Close()
			}
			_ = e.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	c := config{Mode: *mode, Rows: integers(*rows), Clients: integers(*clients), Requests: *requests}
	if *mode == "external" && *dsn == "" {
		printExternalUsage()
		os.Exit(2)
	}
	if len(c.Rows) == 0 || len(c.Clients) == 0 || c.Requests < 1 || (c.Mode != "native-git" && c.Mode != "journal" && c.Mode != "external") {
		fmt.Fprintln(os.Stderr, "invalid mode, positive row/client list, or request count")
		os.Exit(2)
	}
	for _, n := range c.Rows {
		if n < 100 {
			fmt.Fprintln(os.Stderr, "rows must be at least 100")
			os.Exit(2)
		}
	}
	root, err := os.MkdirTemp(*parent, "repodb-scorecard-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r := report{Version: 1, Started: time.Now().UTC(), Config: c, Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Git: commandOutput("git", "--version"), Revision: commandOutput("git", "rev-parse", "HEAD"), WorkingTree: commandOutput("git", "status", "--porcelain"), Root: root}
	if *mode == "external" {
		err = runExternal(&r, *dsn)
	} else {
		err = run(&r)
	}
	if err != nil {
		r.Failure = err.Error()
	}
	var usage unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) == nil {
		r.PeakRSS = int64(usage.Maxrss)
		if runtime.GOOS != "darwin" {
			r.PeakRSS *= 1024
		}
	}
	data, jsonErr := json.MarshalIndent(r, "", "  ")
	if jsonErr == nil {
		jsonErr = os.WriteFile(*output, append(data, '\n'), 0644)
	}
	printReport(r)
	fmt.Printf("\nJSON: %s\nFixtures retained: %s\n", *output, root)
	if jsonErr != nil {
		fmt.Fprintln(os.Stderr, jsonErr)
	}
	if err != nil || jsonErr != nil {
		os.Exit(1)
	}
}

func integers(s string) []int {
	var result []int
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || seen[n] {
			return nil
		}
		seen[n] = true
		result = append(result, n)
	}
	return result
}

func commandOutput(name string, args ...string) string {
	b, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unavailable: %v: %s", err, b)
	}
	return strings.TrimSpace(string(b))
}

func git(dir string, args ...string) error {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	b, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, b)
	}
	return nil
}

// All persistence-specific behavior lives in this setup/publication adapter.
type database struct {
	path    string
	options engine.Options
	e       *engine.Engine
}

func (d *database) open() error {
	var err error
	d.e, err = engine.OpenWithOptions(ctx, d.path, d.options)
	return err
}
func (d *database) publish() error {
	if d.e.WorkingState() != nil {
		_, err := d.e.Checkpoint(ctx, "benchmark publication")
		return err
	}
	return nil
}
func (d *database) sync() error {
	if err := d.publish(); err != nil {
		return err
	}
	_, err := integration.Sync(ctx, d.path, "origin")
	return err
}
func (d *database) exec(q string, args ...any) error {
	s, err := d.e.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Exec(ctx, q, args...)
}
func (d *database) verify(q, value string) error {
	s, err := d.e.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	return expect(s, q, value)
}

func expect(s *engine.Session, q, value string) error {
	r, err := s.Query(ctx, q)
	if err != nil {
		return err
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || fmt.Sprint(r.Rows[0][0]) != value {
		return fmt.Errorf("%s: expected %q, got %v", q, value, r.Rows)
	}
	return nil
}

func directoryBytes(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max(0, int(math.Ceil(p*float64(len(sorted))))-1)]
}

func (r *report) measure(root string, rows int, name string, clients, n int, allowConflict bool, fn func(int, int) error) error {
	fmt.Fprintf(os.Stderr, "%s rows=%d clients=%d requests=%d\n", name, rows, clients, n)
	before, err := directoryBytes(root)
	if err != nil {
		return err
	}
	var mem0, mem1 runtime.MemStats
	runtime.ReadMemStats(&mem0)
	m := measurement{Rows: rows, Name: name, Clients: clients}
	var mu sync.Mutex
	var wg sync.WaitGroup
	startGate := make(chan struct{})
	for worker := 0; worker < clients; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-startGate
			for i := 0; i < n; i++ {
				start := time.Now()
				e := fn(worker, i)
				elapsed := float64(time.Since(start)) / float64(time.Millisecond)
				mu.Lock()
				if e == nil {
					m.Success++
					m.Durations = append(m.Durations, elapsed)
				} else if allowConflict && errors.Is(e, repository.ErrConflict) {
					m.Conflicts++
					m.Rejected = append(m.Rejected, elapsed)
				} else {
					m.Errors = append(m.Errors, e.Error())
				}
				mu.Unlock()
				if e != nil && !(allowConflict && errors.Is(e, repository.ErrConflict)) {
					break
				}
			}
		}(worker)
	}
	start := time.Now()
	close(startGate)
	wg.Wait()
	duration := time.Since(start)
	runtime.ReadMemStats(&mem1)
	after, sizeErr := directoryBytes(root)
	m.WallMS = float64(duration) / float64(time.Millisecond)
	m.Ops = float64(m.Success) / duration.Seconds()
	m.Allocated = mem1.TotalAlloc - mem0.TotalAlloc
	m.Heap = mem1.HeapAlloc
	m.Growth = after - before
	sorted := append([]float64(nil), m.Durations...)
	sort.Float64s(sorted)
	m.P50 = percentile(sorted, .5)
	m.P95 = percentile(sorted, .95)
	m.P99 = percentile(sorted, .99)
	m.Max = percentile(sorted, 1)
	r.Results = append(r.Results, m)
	if sizeErr != nil {
		return sizeErr
	}
	if len(m.Errors) > 0 {
		return fmt.Errorf("%s: %s", name, m.Errors[0])
	}
	if m.Success == 0 {
		return fmt.Errorf("%s: no successful operations", name)
	}
	return nil
}

func run(r *report) error {
	var failures []error
	for _, rows := range r.Config.Rows {
		if err := runSize(r, rows); err != nil {
			failures = append(failures, fmt.Errorf("rows=%d: %w", rows, err))
		}
	}
	return errors.Join(failures...)
}

func runSize(r *report, rows int) error {
	root := filepath.Join(r.Root, strconv.Itoa(rows))
	if err := os.Mkdir(root, 0755); err != nil {
		return err
	}
	remote := filepath.Join(root, "remote.git")
	if err := git(root, "init", "--bare", "--quiet", remote); err != nil {
		return err
	}
	d := &database{path: filepath.Join(root, "local"), options: engine.Options{Persistence: engine.PersistenceMode(r.Config.Mode)}}
	if err := newRepo(d.path, remote); err != nil {
		return err
	}
	if _, err := integration.Enable(ctx, d.path, "origin"); err != nil {
		return err
	}
	m := func(name string, n int, fn func(int, int) error) error {
		return r.measure(root, rows, name, 1, n, false, fn)
	}
	if err := m("open", 1, func(int, int) error { return d.open() }); err != nil {
		return err
	}
	defer func() { _ = d.e.Close() }()
	if err := m("schema", 1, func(int, int) error {
		for _, q := range []string{"CREATE TABLE bench (id BIGINT PRIMARY KEY, value TEXT NOT NULL)", "CREATE TABLE authors (id BIGINT PRIMARY KEY, name TEXT NOT NULL)", "CREATE TABLE marker (id BIGINT PRIMARY KEY, value TEXT NOT NULL)", "INSERT INTO marker VALUES (1,'durable')", "INSERT INTO authors VALUES (1,'author')"} {
			if err := d.exec(q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := m("bulk_load", 1, func(int, int) error {
		s, err := d.e.NewSession()
		if err != nil {
			return err
		}
		defer s.Close()
		tx, err := s.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		for start := 1; start <= rows; start += 500 {
			var q strings.Builder
			q.WriteString("INSERT INTO bench VALUES ")
			for id := start; id <= min(rows, start+499); id++ {
				if id > start {
					q.WriteByte(',')
				}
				fmt.Fprintf(&q, "(%d,'initial-01234567890123456789012345')", id)
			}
			if err := tx.Exec(ctx, q.String()); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}); err != nil {
		return err
	}
	if err := d.verify("SELECT COUNT(*) FROM bench", strconv.Itoa(rows)); err != nil {
		return err
	}
	// Establish the same committed starting dataset before exercising ordinary saves.
	if err := m("initial_publish_sync", 1, func(int, int) error { return d.sync() }); err != nil {
		return err
	}
	s, err := d.e.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	n := r.Config.Requests
	for _, test := range []struct {
		name, q string
		count   int
	}{
		{"point_read", "SELECT value FROM bench WHERE id = 50", 1},
		{"point_miss", "SELECT value FROM bench WHERE id = 0", 0},
		{"range_read", "SELECT * FROM bench WHERE id >= 1 AND id <= 100", 100},
		{"ordered_limit", "SELECT * FROM bench ORDER BY id DESC LIMIT 20", 20},
		{"full_scan", "SELECT * FROM bench", rows},
		{"join_aggregate", "SELECT a.name, COUNT(*) FROM bench b JOIN authors a ON a.id = 1 GROUP BY a.name", 1},
	} {
		if err := m(test.name, n, func(int, int) error {
			result, err := s.Query(ctx, test.q)
			if err == nil && len(result.Rows) != test.count {
				return fmt.Errorf("expected %d rows, got %d", test.count, len(result.Rows))
			}
			return err
		}); err != nil {
			return err
		}
	}
	if err := m("read_transaction", n, func(int, int) error {
		tx, err := s.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		for i := 0; i < 10; i++ {
			if _, err := tx.Query(ctx, "SELECT value FROM bench WHERE id = ?", i+1); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}); err != nil {
		return err
	}
	for _, batch := range []int{1, 10, 100} {
		if err := m(fmt.Sprintf("update_batch_%d", batch), n, func(_ int, i int) error {
			tx, err := s.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			for id := 1; id <= batch; id++ {
				if err := tx.Exec(ctx, "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("batch-%d-generation-%d", batch, i), id); err != nil {
					return err
				}
			}
			return tx.Commit(ctx)
		}); err != nil {
			return err
		}
		if err := expect(s, fmt.Sprintf("SELECT value FROM bench WHERE id = %d", batch), fmt.Sprintf("batch-%d-generation-%d", batch, n-1)); err != nil {
			return err
		}
	}
	if err := m("insert", n, func(_ int, i int) error { return s.Exec(ctx, "INSERT INTO bench VALUES (?, 'inserted')", rows+i+1) }); err != nil {
		return err
	}
	if err := expect(s, "SELECT COUNT(*) FROM bench", strconv.Itoa(rows+n)); err != nil {
		return err
	}
	if err := m("delete", n, func(_ int, i int) error { return s.Exec(ctx, "DELETE FROM bench WHERE id = ?", rows+i+1) }); err != nil {
		return err
	}
	if err := expect(s, "SELECT COUNT(*) FROM bench", strconv.Itoa(rows)); err != nil {
		return err
	}
	if err := m("rollback", n, func(int, int) error {
		tx, err := s.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := tx.Exec(ctx, "UPDATE marker SET value = 'rolled-back' WHERE id = 1"); err != nil {
			return err
		}
		return tx.Rollback(ctx)
	}); err != nil {
		return err
	}
	if err := expect(s, "SELECT value FROM marker WHERE id = 1", "durable"); err != nil {
		return err
	}
	var failures []error
	if err := concurrency(r, root, rows, d); err != nil {
		failures = append(failures, err)
	}
	if err := wire(r, root, rows, d); err != nil {
		return errors.Join(append(failures, err)...)
	}
	// A newly executed process avoids the engine's in-process validation/replay caches.
	if err := m("fresh_process_reopen_verify", min(n, 5), func(int, int) error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, exe, "-verify-repo", d.path, "-mode", r.Config.Mode, "-verify-rows", strconv.Itoa(rows), "-verify-value", fmt.Sprintf("wire-%d", n-1))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("reopen: %w: %s", err, out)
		}
		return nil
	}); err != nil {
		failures = append(failures, err)
	}
	if err := syncWorkloads(r, root, rows, d, remote); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func newRepo(path, remote string) error {
	if err := os.Mkdir(path, 0755); err != nil {
		return err
	}
	if err := git(path, "init", "--quiet", "-b", "main"); err != nil {
		return err
	}
	return git(path, "remote", "add", "origin", remote)
}

func concurrency(r *report, root string, rows int, d *database) error {
	var failures []error
	if err := d.exec("CREATE TABLE counter (id BIGINT PRIMARY KEY, value BIGINT NOT NULL)"); err != nil {
		return err
	}
	if err := d.exec("INSERT INTO counter VALUES (1,0)"); err != nil {
		return err
	}
	for _, clients := range r.Config.Clients {
		sessions := make([]*engine.Session, clients)
		for i := range sessions {
			s, err := d.e.NewSession()
			if err != nil {
				return err
			}
			sessions[i] = s
			defer s.Close()
		}
		var successes atomic.Int64
		err := r.measure(root, rows, "mixed_80read_20write", clients, r.Config.Requests, true, func(w, i int) error {
			if i%5 != 0 {
				_, err := sessions[w].Query(ctx, "SELECT value FROM bench WHERE id = ?", 1+(i+w)%rows)
				return err
			}
			err := sessions[w].Exec(ctx, "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("clients-%d-worker-%d-request-%d", clients, w, i), 1+w%rows)
			if errors.Is(err, repository.ErrConflict) {
				if cleanup := sessions[w].Exec(ctx, "ROLLBACK"); cleanup != nil {
					return fmt.Errorf("rollback rejected write: %v (original: %v)", cleanup, err)
				}
			}
			if err == nil {
				successes.Add(1)
			}
			return err
		})
		if err != nil {
			return err
		}
		if successes.Load() == 0 {
			return errors.New("mixed workload committed no writes")
		}
		if err := d.exec("UPDATE counter SET value = 0 WHERE id = 1"); err != nil {
			return err
		}
		successes.Store(0)
		err = r.measure(root, rows, "contended_increment", clients, r.Config.Requests, true, func(w, i int) error {
			err := sessions[w].Exec(ctx, "UPDATE counter SET value = value + 1 WHERE id = 1")
			if err == nil {
				successes.Add(1)
			}
			if errors.Is(err, repository.ErrConflict) {
				if cleanup := sessions[w].Exec(ctx, "ROLLBACK"); cleanup != nil {
					return fmt.Errorf("rollback rejected increment: %v (original: %v)", cleanup, err)
				}
			}
			return err
		})
		if err != nil {
			return err
		}
		if err := d.verify("SELECT value FROM counter WHERE id = 1", strconv.FormatInt(successes.Load(), 10)); err != nil {
			err = fmt.Errorf("lost-update check: %w", err)
			r.Results[len(r.Results)-1].VerificationError = err.Error()
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func wire(r *report, root string, rows int, d *database) error {
	srv, err := server.New(server.Config{Address: "127.0.0.1:0", Repository: d.e.Repository(), Persistence: d.options.Persistence})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	defer func() { _ = srv.Close(); <-done }()
	c, err := client.Open(client.Config{Address: srv.Address(), Database: "repodb"})
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Ping(ctx); err != nil {
		return err
	}
	if err := r.measure(root, rows, "mysql_point_read", 1, r.Config.Requests, false, func(int, int) error {
		result, err := c.Query(ctx, "SELECT value FROM marker WHERE id = 1")
		if err == nil && (len(result.Rows) != 1 || result.Rows[0][0] == nil || *result.Rows[0][0] != "durable") {
			return errors.New("wire read mismatch")
		}
		return err
	}); err != nil {
		return err
	}
	if err := r.measure(root, rows, "mysql_update", 1, r.Config.Requests, false, func(_ int, i int) error {
		_, err := c.Exec(ctx, fmt.Sprintf("UPDATE bench SET value = 'wire-%d' WHERE id = 1", i))
		return err
	}); err != nil {
		return err
	}
	return d.verify("SELECT value FROM bench WHERE id = 1", fmt.Sprintf("wire-%d", r.Config.Requests-1))
}

func syncWorkloads(r *report, root string, rows int, d *database, remote string) error {
	m := func(name string, n int, fn func(int, int) error) error {
		return r.measure(root, rows, name, 1, n, false, fn)
	}
	if err := m("publish_sync_after_writes", 1, func(int, int) error { return d.sync() }); err != nil {
		return err
	}
	peer := &database{path: filepath.Join(root, "peer"), options: d.options}
	if err := m("clone_enable_open", 1, func(int, int) error {
		if err := newRepo(peer.path, remote); err != nil {
			return err
		}
		if _, err := integration.Enable(ctx, peer.path, "origin"); err != nil {
			return err
		}
		return peer.open()
	}); err != nil {
		return err
	}
	defer peer.e.Close()
	if err := peer.verify("SELECT COUNT(*) FROM bench", strconv.Itoa(rows)); err != nil {
		return err
	}
	if err := m("sync_unchanged", r.Config.Requests, func(int, int) error { return d.sync() }); err != nil {
		return err
	}
	// Independent scenarios share a committed seed, never a possibly failed journal.
	seedOptions := d.options
	resetPair := func(name string) error {
		var err error
		d, peer, err = syncPair(root, name, remote, seedOptions)
		return err
	}
	var failures []error
	if err := resetPair("roundtrip"); err != nil {
		return err
	}
	defer d.e.Close()
	defer peer.e.Close()
	// Each iteration starts converged; timing includes local saves and both directions.
	if err := m("edit_sync_roundtrip", r.Config.Requests, func(_ int, i int) error {
		v := fmt.Sprintf("roundtrip-%d", i)
		if err := d.exec("UPDATE bench SET value = ? WHERE id = 1", v); err != nil {
			return err
		}
		if err := d.sync(); err != nil {
			return err
		}
		if err := peer.sync(); err != nil {
			return err
		}
		return peer.verify("SELECT value FROM bench WHERE id = 1", v)
	}); err != nil {
		failures = append(failures, err)
	}
	if err := resetPair("merge"); err != nil {
		return errors.Join(append(failures, err)...)
	}
	defer d.e.Close()
	defer peer.e.Close()
	if err := m("divergent_edit_merge", r.Config.Requests, func(_ int, i int) error {
		v := fmt.Sprintf("merge-%d", i)
		if err := d.exec("UPDATE bench SET value = ? WHERE id = 1", v); err != nil {
			return err
		}
		if err := peer.exec("UPDATE bench SET value = ? WHERE id = 2", v); err != nil {
			return err
		}
		if err := d.sync(); err != nil {
			return err
		}
		if err := peer.sync(); err != nil {
			return err
		}
		if err := d.sync(); err != nil {
			return err
		}
		for _, db := range []*database{d, peer} {
			for _, id := range []int{1, 2} {
				if err := db.verify(fmt.Sprintf("SELECT value FROM bench WHERE id = %d", id), v); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		failures = append(failures, err)
	}
	if err := resetPair("conflict"); err != nil {
		return errors.Join(append(failures, err)...)
	}
	defer d.e.Close()
	defer peer.e.Close()
	err := m("conflict_resolve_roundtrip", r.Config.Requests, func(_ int, i int) error {
		local, other := fmt.Sprintf("local-%d", i), fmt.Sprintf("peer-%d", i)
		if err := d.exec("UPDATE bench SET value = ? WHERE id = 1", local); err != nil {
			return err
		}
		if err := peer.exec("UPDATE bench SET value = ? WHERE id = 1", other); err != nil {
			return err
		}
		if err := d.sync(); err != nil {
			return err
		}
		err := peer.sync()
		var conflict *integration.MergeConflictError
		if !errors.As(err, &conflict) {
			return fmt.Errorf("expected merge conflict, got %v", err)
		}
		for _, item := range conflict.Set.Unresolved() {
			if _, err := integration.Resolve(ctx, peer.path, "origin", item.ID, engine.TakeRemote); err != nil {
				return err
			}
		}
		if err := d.sync(); err != nil {
			return err
		}
		for _, db := range []*database{d, peer} {
			if err := db.verify("SELECT value FROM bench WHERE id = 1", local); err != nil {
				return err
			}
		}
		return nil
	})
	return errors.Join(append(failures, err)...)
}

func syncPair(root, name, seed string, options engine.Options) (*database, *database, error) {
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0755); err != nil {
		return nil, nil, err
	}
	remote := filepath.Join(dir, "remote.git")
	if err := git(dir, "init", "--bare", "--quiet", remote); err != nil {
		return nil, nil, err
	}
	if err := git(remote, "fetch", "--quiet", seed, repository.DataRef+":"+repository.DataRef); err != nil {
		return nil, nil, err
	}
	var dbs []*database
	for _, name := range []string{"a", "b"} {
		d := &database{path: filepath.Join(dir, name), options: options}
		err := newRepo(d.path, remote)
		if err == nil {
			_, err = integration.Enable(ctx, d.path, "origin")
		}
		if err == nil {
			err = d.open()
		}
		if err != nil {
			for _, opened := range dbs {
				_ = opened.e.Close()
			}
			return nil, nil, err
		}
		dbs = append(dbs, d)
	}
	return dbs[0], dbs[1], nil
}

func printReport(r report) {
	fmt.Printf("\nRepoDB scorecard v%d | %s | %s | %s\n", r.Version, r.Config.Mode, r.Platform, r.Revision)
	fmt.Println("Latency is per successful request, milliseconds. Conflicts are rejected attempts; no retries. Small samples do not establish stable tails.")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "Rows\tWorkload\tClients\tOK\tConflict\tError\tStatus\tOps/s\tp50\tp95\tp99\tMax\tGrowth KiB")
	for _, m := range r.Results {
		status := "PASS"
		if len(m.Errors) > 0 || m.VerificationError != "" || m.Success == 0 {
			status = "FAIL"
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%d\t%d\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f\n", m.Rows, m.Name, m.Clients, m.Success, m.Conflicts, len(m.Errors), status, m.Ops, m.P50, m.P95, m.P99, m.Max, float64(m.Growth)/1024)
	}
	_ = w.Flush()
	fmt.Printf("Process peak RSS: %.1f MiB (excludes Git/verification children)\n", float64(r.PeakRSS)/(1024*1024))
	if r.Failure != "" {
		fmt.Printf("FAILED: %s\n", r.Failure)
	}
}
