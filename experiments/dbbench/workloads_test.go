package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/nicbet/repodb/common/repository"
)

func TestParseWorkloads(t *testing.T) {
	for in, want := range map[string][]string{
		"core,append,mutable": {"core", "append", "mutable"},
		"mutable,append":      {"append", "mutable"},
		"core":                {"core"},
	} {
		got, err := parseWorkloads(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseWorkloads(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "core,,append", "core,core", "events", "Core"} {
		if _, err := parseWorkloads(in); err == nil {
			t.Errorf("parseWorkloads(%q) accepted", in)
		}
	}
}

func TestFixturesAreDeterministic(t *testing.T) {
	firstBody, secondBody := commentBody(7), commentBody(7)
	if !reflect.DeepEqual(makeEvent(42), makeEvent(42)) || !reflect.DeepEqual(makeIssue(42), makeIssue(42)) || firstBody != secondBody {
		t.Fatal("same id produced different rows")
	}
	if makeEvent(1).payload == makeEvent(2).payload || makeIssue(1).body == makeIssue(2).body {
		t.Fatal("different ids produced the same content")
	}
	kinds := map[string]int{}
	for id := int64(1); id <= 1000; id++ {
		ev := makeEvent(id)
		if len(ev.payload) != 256 || !json.Valid([]byte(ev.payload)) {
			t.Fatalf("event %d payload: %d bytes, valid JSON %v", id, len(ev.payload), json.Valid([]byte(ev.payload)))
		}
		if id > 1 && ev.ts <= eventTS(id-1) {
			t.Fatalf("event %d: ts not strictly increasing", id)
		}
		if want := (id-1)/eventsPerSession + 1; ev.session != want {
			t.Fatalf("event %d: session %d, want %d", id, ev.session, want)
		}
		kinds[ev.kind]++
		is := makeIssue(id)
		if len(is.body) != 1024 || len(is.title) > 120 {
			t.Fatalf("issue %d: body %d bytes, title %d bytes", id, len(is.body), len(is.title))
		}
	}
	if len(kinds) != eventKinds {
		t.Fatalf("got %d kinds, want %d", len(kinds), eventKinds)
	}
}

func TestHotPickerBoundsAndSkew(t *testing.T) {
	const n = 1000
	h := newHotPicker(3, n)
	recent := 0
	for i := 0; i < 10000; i++ {
		id := h.next()
		if id < 1 || id > n {
			t.Fatalf("id %d outside [1, %d]", id, n)
		}
		if id > n-n/5 {
			recent++
		}
	}
	// Zipf s=1.1 puts most draws on the newest fifth of the issues.
	if recent < 7000 {
		t.Fatalf("only %d of 10000 draws hit the newest 20%%", recent)
	}
	a, b := newHotPicker(9, n), newHotPicker(9, n)
	for i := 0; i < 100; i++ {
		if a.next() != b.next() {
			t.Fatal("same seed produced different draws")
		}
	}
}

// fakeBackend counts syncs and reports a fixture that grows 100 bytes per
// call; its peer is itself.
type fakeBackend struct {
	sync_, journal bool
	syncs_, bytes  int64
	dir            string
}

func (b *fakeBackend) open() error            { return nil }
func (b *fakeBackend) close() error           { return nil }
func (b *fakeBackend) connect() (conn, error) { return fakeConn{}, nil }
func (b *fakeBackend) root() string           { return b.dir }
func (b *fakeBackend) syncs() bool            { return b.sync_ }
func (b *fakeBackend) sync() error            { b.syncs_++; return nil }
func (b *fakeBackend) housekeeping() error    { return nil }
func (b *fakeBackend) peer() (backend, error) { return b, nil }
func (b *fakeBackend) journalBytes() (int64, bool) {
	return 10 * b.syncs_, b.journal
}
func (b *fakeBackend) fixtureBytes() (int64, bool) {
	if !b.sync_ {
		return 0, false
	}
	b.bytes += 100
	return b.bytes, true
}

type fakeConn struct{}

func (fakeConn) exec(string, ...any) error { return nil }
func (fakeConn) query(string, ...any) ([][]string, error) {
	return [][]string{{"1"}}, nil
}
func (c fakeConn) tx(fn func(conn) error) error { return fn(c) }
func (fakeConn) close() error                   { return nil }

func TestGrowthRecordsOnePointPerRound(t *testing.T) {
	b := &fakeBackend{sync_: true, journal: true, dir: t.TempDir()}
	r := &report{Config: config{GrowthRounds: 3}}
	e := env{r: r, group: "append", rows: 1000, b: b}
	burst := func(round int) ([]float64, error) { return []float64{float64(round), 2 * float64(round)}, nil }
	if err := growth(e, fakeConn{}, burst, func() int { return 1234 }, []string{"SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if len(r.Series) != 3 || b.syncs_ != 3 {
		t.Fatalf("got %d points and %d syncs, want 3 each", len(r.Series), b.syncs_)
	}
	for i, p := range r.Series {
		if p.Group != "append" || p.Rows != 1000 || p.Round != i+1 || p.TableRows != 1234 || p.WriteRequests != 2 {
			t.Fatalf("point %d: %+v", i, p)
		}
		if p.SyncMS == nil || p.FixtureBytes == nil || *p.FixtureDelta != 100 || *p.JournalAfter != *p.JournalBefore+10 {
			t.Fatalf("point %d: missing sync, size or journal data: %+v", i, p)
		}
	}
	if len(r.Results) != 1 || r.Results[0].Name != "append_peer_pull_after_growth" || r.Results[0].Group != "append" {
		t.Fatalf("expected one peer pull measurement, got %+v", r.Results)
	}

	// Without sync (external servers) rounds still record writes, but no
	// sync, size, journal or peer data.
	b = &fakeBackend{dir: t.TempDir()}
	r = &report{Config: config{GrowthRounds: 2}}
	if err := growth(env{r: r, group: "mutable", rows: 1000, b: b}, fakeConn{}, burst, func() int { return 1000 }, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.Series) != 2 || len(r.Results) != 0 {
		t.Fatalf("got %d points and %d results", len(r.Series), len(r.Results))
	}
	if p := r.Series[0]; p.SyncMS != nil || p.FixtureBytes != nil || p.JournalBefore != nil {
		t.Fatalf("external point has sync data: %+v", p)
	}
}

func TestGrowthFailsOnPeerDivergence(t *testing.T) {
	b := &fakeBackend{sync_: true, dir: t.TempDir()}
	r := &report{Config: config{GrowthRounds: 1}}
	e := env{r: r, group: "append", rows: 1000, b: b}
	diverged := &divergentBackend{fakeBackend: b}
	e.b = diverged
	err := growth(e, fakeConn{}, func(int) ([]float64, error) { return nil, nil }, func() int { return 0 }, []string{"SELECT 1"})
	if err == nil || r.Results[0].VerificationError == "" {
		t.Fatalf("divergence not reported: %v %+v", err, r.Results)
	}
}

type divergentBackend struct{ *fakeBackend }

func (b *divergentBackend) peer() (backend, error) { return peerBackend{b.fakeBackend}, nil }

type peerBackend struct{ *fakeBackend }

func (peerBackend) connect() (conn, error) { return otherConn{}, nil }

type otherConn struct{ fakeConn }

func (otherConn) query(string, ...any) ([][]string, error) { return [][]string{{"2"}}, nil }

func TestExternalDeadlockIsConflict(t *testing.T) {
	for _, number := range []uint16{1213, 1205} {
		if err := externalConflict(&mysql.MySQLError{Number: number}); !errors.Is(err, repository.ErrConflict) {
			t.Errorf("MySQL error %d not reported as a conflict: %v", number, err)
		}
	}
	for _, err := range []error{nil, errors.New("syntax"), &mysql.MySQLError{Number: 1064}} {
		if errors.Is(externalConflict(err), repository.ErrConflict) {
			t.Errorf("%v reported as a conflict", err)
		}
	}
}
