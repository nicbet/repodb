package engine_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

// openRebaseTable returns a journal engine with table t (id, unique u,
// indexed n) holding rows 1 and 2.
func openRebaseTable(t *testing.T) (string, *engine.Engine) {
	t.Helper()
	root := gitRepository(t)
	if _, err := repository.Init(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	eng := openJournal(t, root)
	t.Cleanup(func() { eng.Close() })
	execAll(t, eng,
		"CREATE TABLE t (id BIGINT PRIMARY KEY, u BIGINT, n VARCHAR(20), UNIQUE KEY u_idx (u), KEY n_idx (n))",
		"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b')",
	)
	return root, eng
}

// begin starts a transaction and runs statements in it, which fixes its
// snapshot.
func begin(t *testing.T, eng *engine.Engine, statements ...string) *engine.Tx {
	t.Helper()
	session, _ := eng.NewSession()
	t.Cleanup(func() { session.Close() })
	tx, err := session.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range statements {
		if err := tx.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return tx
}

func TestJournalDisjointWritersBothCommit(t *testing.T) {
	ctx := context.Background()
	root, eng := openRebaseTable(t)
	a := begin(t, eng, "UPDATE t SET n = 'a2' WHERE id = 1", "INSERT INTO t VALUES (3, 30, 'c')")
	b := begin(t, eng, "UPDATE t SET u = 21, n = 'b2' WHERE id = 2", "INSERT INTO t VALUES (4, 40, 'c')")
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("disjoint stale commit = %v", err)
	}
	check := func(eng *engine.Engine) {
		t.Helper()
		if got := queryIDs(t, eng, "SELECT id, u, n FROM t ORDER BY id"); got != "[[1 10 a2] [2 21 b2] [3 30 c] [4 40 c]]" {
			t.Fatalf("rows = %s", got)
		}
		// Both transactions' index edits are visible.
		if got := queryIDs(t, eng, "SELECT id FROM t WHERE n = 'c' ORDER BY id"); got != "[[3] [4]]" {
			t.Fatalf("n='c' = %s", got)
		}
		for u, want := range map[int]string{20: "[]", 21: "[[2]]", 30: "[[3]]", 40: "[[4]]"} {
			if got := queryIDs(t, eng, fmt.Sprintf("SELECT id FROM t WHERE u = %d", u)); got != want {
				t.Fatalf("u=%d = %s, want %s", u, got, want)
			}
		}
		expectDuplicate(t, eng, "INSERT INTO t VALUES (5, 30, 'x')")
		expectDuplicate(t, eng, "INSERT INTO t VALUES (5, 21, 'x')")
	}
	check(eng)
	reopened := openJournal(t, root)
	defer reopened.Close()
	check(reopened)
}

func TestJournalSameRowWritersConflict(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ first, second string }{
		{"UPDATE t SET n = 'x' WHERE id = 1", "UPDATE t SET n = 'y' WHERE id = 1"},
		{"INSERT INTO t VALUES (3, 30, 'x')", "INSERT INTO t VALUES (3, 31, 'y')"},
		{"DELETE FROM t WHERE id = 1", "UPDATE t SET n = 'y' WHERE id = 1"},
		{"UPDATE t SET id = 5 WHERE id = 1", "INSERT INTO t VALUES (5, 50, 'y')"},
	} {
		t.Run(tc.first+" / "+tc.second, func(t *testing.T) {
			_, eng := openRebaseTable(t)
			a, b := begin(t, eng, tc.first), begin(t, eng, tc.second)
			if err := a.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.Commit(ctx); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("second commit = %v, want ErrConflict", err)
			}
		})
	}
}

func TestJournalUniqueCollisionAcrossRowsConflicts(t *testing.T) {
	ctx := context.Background()
	_, eng := openRebaseTable(t)
	for _, tc := range []struct{ first, second string }{
		{"INSERT INTO t VALUES (3, 99, 'x')", "INSERT INTO t VALUES (4, 99, 'y')"},
		{"UPDATE t SET u = 77 WHERE id = 1", "UPDATE t SET u = 77 WHERE id = 2"},
	} {
		a, b := begin(t, eng, tc.first), begin(t, eng, tc.second)
		if err := a.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.Commit(ctx); !errors.Is(err, repository.ErrConflict) {
			t.Fatalf("%s / %s: second commit = %v, want ErrConflict", tc.first, tc.second, err)
		}
	}
	// NULLs never collide in a unique index.
	a, b := begin(t, eng, "INSERT INTO t VALUES (5, NULL, 'x')"), begin(t, eng, "INSERT INTO t VALUES (6, NULL, 'y')")
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("second NULL insert = %v", err)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 99 OR u = 77 OR u IS NULL ORDER BY id"); got != "[[1] [3] [5] [6]]" {
		t.Fatalf("rows = %s", got)
	}
}

func TestJournalSchemaChangesConflictWithWriters(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		ddl      string
		conflict bool
	}{
		{"ALTER TABLE t ADD COLUMN extra BIGINT", true},
		{"DROP TABLE t", true},
		{"CREATE INDEX n2_idx ON t (n, u)", true},
		{"CREATE TABLE other (id BIGINT PRIMARY KEY)", false},
	} {
		t.Run(tc.ddl, func(t *testing.T) {
			_, eng := openRebaseTable(t)
			writer := begin(t, eng, "INSERT INTO t VALUES (3, 30, 'c')")
			execAll(t, eng, tc.ddl)
			err := writer.Commit(ctx)
			if tc.conflict != errors.Is(err, repository.ErrConflict) || (!tc.conflict && err != nil) {
				t.Fatalf("commit after %s = %v, want conflict %v", tc.ddl, err, tc.conflict)
			}
		})
	}
}

func TestJournalCheckpointSinceSnapshotConflicts(t *testing.T) {
	ctx := context.Background()
	_, eng := openRebaseTable(t)
	writer := begin(t, eng, "INSERT INTO t VALUES (3, 30, 'c')")
	execAll(t, eng, "INSERT INTO t VALUES (4, 40, 'd')")
	if _, err := eng.Checkpoint(ctx, "checkpoint"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("commit across a checkpoint = %v, want ErrConflict", err)
	}
}

func TestSeparateJournalEnginesRebaseDisjointWriters(t *testing.T) {
	ctx := context.Background()
	root, a := openRebaseTable(t)
	b := openJournal(t, root)
	defer b.Close()
	stale := begin(t, b, "INSERT INTO t VALUES (4, 40, 'd')")
	execAll(t, a, "INSERT INTO t VALUES (3, 30, 'c')", "UPDATE t SET n = 'z' WHERE id = 1")
	if err := stale.Commit(ctx); err != nil {
		t.Fatalf("cross-engine disjoint commit = %v", err)
	}
	for _, eng := range []*engine.Engine{a, b} {
		if got := queryIDs(t, eng, "SELECT id, n FROM t ORDER BY id"); got != "[[1 z] [2 b] [3 c] [4 d]]" {
			t.Fatalf("rows = %s", got)
		}
	}
}

// Rounds of concurrent transactions, each starting on the same snapshot and
// committing in random order, through one or two engines. A commit must be
// rejected exactly when it overlaps one committed earlier in the round (the
// same row, or the same unique value). Indexes must match the model after each
// round, which checks the index-edit cache after rebased commits.
func TestJournalRebaseMatchesModel(t *testing.T) {
	t.Run("one-engine", func(t *testing.T) { runRebaseModel(t, 1) })
	t.Run("two-engines", func(t *testing.T) { runRebaseModel(t, 2) })
}

func runRebaseModel(t *testing.T, engineCount int) {
	ctx := context.Background()
	root, first := openRebaseTable(t)
	engines := []*engine.Engine{first}
	for len(engines) < engineCount {
		eng := openJournal(t, root)
		defer eng.Close()
		engines = append(engines, eng)
	}
	type row struct {
		u int64
		n string
	}
	model := map[int64]row{1: {10, "a"}, 2: {20, "b"}}
	type txn struct {
		tx    *engine.Tx
		stmt  string
		id    int64 // the row written, or -1
		claim int64 // the unique value claimed, or -1
		apply func(map[int64]row)
	}
	rng := rand.New(rand.NewPCG(7, 11))
	rebased := 0
	for round := 0; round < 60; round++ {
		if round%20 == 19 {
			if _, err := engines[0].Checkpoint(ctx, fmt.Sprint("round ", round)); err != nil {
				t.Fatal(err)
			}
		}
		var txns []*txn
		for i := 0; i < 3; i++ {
			id, u, n := int64(rng.IntN(12)), int64(rng.IntN(10)), fmt.Sprint("n", rng.IntN(4))
			current, exists := model[id]
			taken := func(except int64) bool {
				for other, r := range model {
					if other != except && r.u == u {
						return true
					}
				}
				return false
			}
			x := &txn{id: -1, claim: -1}
			var wantErr bool
			switch op := rng.IntN(3); op {
			case 0:
				x.stmt = fmt.Sprintf("INSERT INTO t VALUES (%d, %d, '%s')", id, u, n)
				wantErr = exists || taken(-1)
				if !wantErr {
					x.id, x.claim = id, u
					x.apply = func(m map[int64]row) { m[id] = row{u, n} }
				}
			case 1:
				x.stmt = fmt.Sprintf("UPDATE t SET u = %d, n = '%s' WHERE id = %d", u, n, id)
				wantErr = exists && taken(id)
				if exists && !wantErr && (current.u != u || current.n != n) {
					x.id = id
					if current.u != u {
						x.claim = u
					}
					x.apply = func(m map[int64]row) { m[id] = row{u, n} }
				}
			default:
				x.stmt = fmt.Sprintf("DELETE FROM t WHERE id = %d", id)
				if exists {
					x.id = id
					x.apply = func(m map[int64]row) { delete(m, id) }
				}
			}
			eng := engines[rng.IntN(len(engines))]
			session, _ := eng.NewSession()
			defer session.Close()
			tx, err := session.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Exec(ctx, x.stmt); (err != nil) != wantErr {
				t.Fatalf("round %d: %s: err = %v, want error %v", round, x.stmt, err, wantErr)
			}
			x.tx = tx
			txns = append(txns, x)
		}
		rng.Shuffle(len(txns), func(i, j int) { txns[i], txns[j] = txns[j], txns[i] })
		var committed []*txn
		for _, x := range txns {
			overlaps := false
			for _, c := range committed {
				if (x.id >= 0 && x.id == c.id) || (x.claim >= 0 && x.claim == c.claim) {
					overlaps = true
				}
			}
			err := x.tx.Commit(ctx)
			if err != nil && !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("round %d: commit %s: %v", round, x.stmt, err)
			}
			if overlaps != (err != nil) {
				t.Fatalf("round %d: commit %s = %v, overlaps an earlier commit: %v", round, x.stmt, err, overlaps)
			}
			if err == nil && x.apply != nil {
				if len(committed) > 0 {
					rebased++
				}
				x.apply(model)
				committed = append(committed, x)
			}
		}

		check := engines[rng.IntN(len(engines))]
		var ids []int64
		for id := range model {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		want := "["
		for i, id := range ids {
			if i > 0 {
				want += " "
			}
			want += fmt.Sprintf("[%d %d %s]", id, model[id].u, model[id].n)
		}
		want += "]"
		if got := queryIDs(t, check, "SELECT id, u, n FROM t ORDER BY id"); got != want {
			t.Fatalf("round %d: rows = %s, want %s", round, got, want)
		}
		for u := int64(0); u < 10; u++ {
			want := "[]"
			for id, r := range model {
				if r.u == u {
					want = fmt.Sprintf("[[%d]]", id)
				}
			}
			if got := queryIDs(t, check, fmt.Sprintf("SELECT id FROM t WHERE u = %d", u)); got != want {
				t.Fatalf("round %d: u=%d -> %s, want %s", round, u, got, want)
			}
		}
		for i := 0; i < 4; i++ {
			name := fmt.Sprint("n", i)
			want := "["
			for _, id := range ids {
				if model[id].n == name {
					if len(want) > 1 {
						want += " "
					}
					want += fmt.Sprintf("[%d]", id)
				}
			}
			want += "]"
			if got := queryIDs(t, check, fmt.Sprintf("SELECT id FROM t WHERE n = '%s' ORDER BY id", name)); got != want {
				t.Fatalf("round %d: n=%s -> %s, want %s", round, name, got, want)
			}
		}
	}
	if rebased == 0 {
		t.Fatal("no commit was rebased")
	}
}
