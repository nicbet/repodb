package engine_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

// openCheckpointedIndexedTable returns a journal engine whose table t has
// persisted unique (u) and non-unique (n) index trees: rows (1, 10, 'a') and
// (2, 20, 'b') are checkpointed before any journal-only writes happen.
func openCheckpointedIndexedTable(t *testing.T) (string, *engine.Engine) {
	t.Helper()
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng := openJournal(t, root)
	execAll(t, eng,
		"CREATE TABLE t (id BIGINT PRIMARY KEY, u BIGINT, n VARCHAR(20), UNIQUE KEY u_idx (u), KEY n_idx (n))",
		"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b')",
	)
	if _, err := eng.Checkpoint(ctx, "persist index trees"); err != nil {
		t.Fatal(err)
	}
	return root, eng
}

func openJournal(t *testing.T, root string) *engine.Engine {
	t.Helper()
	eng, err := engine.OpenWithOptions(context.Background(), root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// execAll runs each statement in its own session, so each is a separate
// journal transaction.
func execAll(t *testing.T, eng *engine.Engine, statements ...string) {
	t.Helper()
	for _, stmt := range statements {
		session, _ := eng.NewSession()
		err := session.Exec(context.Background(), stmt)
		_ = session.Close()
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func queryIDs(t *testing.T, eng *engine.Engine, query string) string {
	t.Helper()
	session, _ := eng.NewSession()
	defer session.Close()
	result, err := session.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return fmt.Sprint(result.Rows)
}

func expectDuplicate(t *testing.T, eng *engine.Engine, stmt string) {
	t.Helper()
	session, _ := eng.NewSession()
	defer session.Close()
	if err := session.Exec(context.Background(), stmt); err == nil {
		t.Fatalf("%s: expected duplicate key error", stmt)
	}
}

func TestJournalInsertVisibleThroughPersistedIndexes(t *testing.T) {
	root, eng := openCheckpointedIndexedTable(t)
	execAll(t, eng, "INSERT INTO t VALUES (3, 30, 'c')")

	check := func(eng *engine.Engine) {
		t.Helper()
		if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 30"); got != "[[3]]" {
			t.Errorf("unique lookup u=30 = %s, want [[3]]", got)
		}
		if got := queryIDs(t, eng, "SELECT id FROM t WHERE n = 'c'"); got != "[[3]]" {
			t.Errorf("secondary lookup n='c' = %s, want [[3]]", got)
		}
		if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 10"); got != "[[1]]" {
			t.Errorf("checkpointed lookup u=10 = %s, want [[1]]", got)
		}
		expectDuplicate(t, eng, "INSERT INTO t VALUES (4, 30, 'd')")
	}
	check(eng)
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openJournal(t, root)
	defer reopened.Close()
	check(reopened)
}

func TestJournalUpdateMovesPersistedIndexEntries(t *testing.T) {
	_, eng := openCheckpointedIndexedTable(t)
	defer eng.Close()
	execAll(t, eng, "UPDATE t SET u = 11, n = 'z' WHERE id = 1")

	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 10"); got != "[]" {
		t.Errorf("old unique value u=10 = %s, want []", got)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE n = 'a'"); got != "[]" {
		t.Errorf("old secondary value n='a' = %s, want []", got)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 11"); got != "[[1]]" {
		t.Errorf("new unique value u=11 = %s, want [[1]]", got)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE n = 'z'"); got != "[[1]]" {
		t.Errorf("new secondary value n='z' = %s, want [[1]]", got)
	}
	expectDuplicate(t, eng, "INSERT INTO t VALUES (5, 11, 'e')")
	// The old unique value is free again.
	execAll(t, eng, "INSERT INTO t VALUES (6, 10, 'f')")
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 10"); got != "[[6]]" {
		t.Errorf("reused unique value u=10 = %s, want [[6]]", got)
	}
}

func TestJournalDeleteRemovesPersistedIndexEntries(t *testing.T) {
	_, eng := openCheckpointedIndexedTable(t)
	defer eng.Close()
	execAll(t, eng, "DELETE FROM t WHERE id = 2")

	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 20"); got != "[]" {
		t.Errorf("deleted unique value u=20 = %s, want []", got)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE n = 'b'"); got != "[]" {
		t.Errorf("deleted secondary value n='b' = %s, want []", got)
	}
	execAll(t, eng, "INSERT INTO t VALUES (7, 20, 'b')")
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 20"); got != "[[7]]" {
		t.Errorf("reused unique value u=20 = %s, want [[7]]", got)
	}
}

func TestJournalMixedPersistedAndNewIndexes(t *testing.T) {
	_, eng := openCheckpointedIndexedTable(t)
	defer eng.Close()
	execAll(t, eng,
		"INSERT INTO t VALUES (3, 30, 'c')",
		"CREATE UNIQUE INDEX n_uniq ON t (n)",
		"INSERT INTO t VALUES (4, 40, 'd')",
	)
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 30"); got != "[[3]]" {
		t.Errorf("persisted index u=30 = %s, want [[3]]", got)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 40"); got != "[[4]]" {
		t.Errorf("persisted index u=40 = %s, want [[4]]", got)
	}
	expectDuplicate(t, eng, "INSERT INTO t VALUES (8, 30, 'x')")
	expectDuplicate(t, eng, "INSERT INTO t VALUES (9, 99, 'c')")
}

// BenchmarkJournalIndexedInsertWithPending measures one single-row journal
// transaction on a table with persisted index trees and many pending edits,
// the case that derives index edits from the row overlay.
func BenchmarkJournalIndexedInsertWithPending(b *testing.B) {
	for _, pending := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("pending=%d", pending), func(b *testing.B) {
			ctx := context.Background()
			root := gitRepository(b)
			if _, err := repository.Init(ctx, root); err != nil {
				b.Fatal(err)
			}
			eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
			if err != nil {
				b.Fatal(err)
			}
			defer eng.Close()
			session, _ := eng.NewSession()
			defer session.Close()
			if err := session.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, u BIGINT, n VARCHAR(20), UNIQUE KEY u_idx (u), KEY n_idx (n))"); err != nil {
				b.Fatal(err)
			}
			tx, _ := session.Begin(ctx)
			for i := 0; i < 10000; i++ {
				if err := tx.Exec(ctx, "INSERT INTO t VALUES (?, ?, ?)", i, i, fmt.Sprint("n", i%100)); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				b.Fatal(err)
			}
			if _, err := eng.Checkpoint(ctx, "base"); err != nil {
				b.Fatal(err)
			}
			tx, _ = session.Begin(ctx)
			for i := 0; i < pending; i++ {
				if err := tx.Exec(ctx, "UPDATE t SET n = ? WHERE id = ?", fmt.Sprint("p", i), i); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := session.Exec(ctx, "INSERT INTO t VALUES (?, ?, 'x')", 100000+i, 100000+i); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestJournalIndexesMatchModel applies a deterministic random mix of inserts,
// updates and deletes, with occasional checkpoints and engine reopens (which
// drop the index-edit cache), and checks every unique and secondary index
// lookup against an in-memory model. The two-engine variant writes and reads
// through independent engines, as separate processes would.
func TestJournalIndexesMatchModel(t *testing.T) {
	t.Run("one-engine", func(t *testing.T) { runIndexModel(t, 1) })
	t.Run("two-engines", func(t *testing.T) { runIndexModel(t, 2) })
}

func runIndexModel(t *testing.T, engineCount int) {
	ctx := context.Background()
	root, first := openCheckpointedIndexedTable(t)
	engines := []*engine.Engine{first}
	for len(engines) < engineCount {
		engines = append(engines, openJournal(t, root))
	}
	defer func() {
		for _, eng := range engines {
			_ = eng.Close()
		}
	}()

	type row struct {
		u int64
		n string
	}
	model := map[int64]row{1: {10, "a"}, 2: {20, "b"}}
	uniqueTaken := func(u int64, except int64) bool {
		for id, r := range model {
			if id != except && r.u == u {
				return true
			}
		}
		return false
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for step := 0; step < 150; step++ {
		pick := rng.IntN(len(engines))
		if rng.IntN(15) == 0 {
			if err := engines[pick].Close(); err != nil {
				t.Fatal(err)
			}
			engines[pick] = openJournal(t, root)
		}
		eng := engines[pick]
		session, _ := eng.NewSession()
		id := int64(rng.IntN(40))
		u := int64(rng.IntN(60))
		n := fmt.Sprint("n", rng.IntN(8))
		_, exists := model[id]
		var stmt string
		var wantErr bool
		switch op := rng.IntN(10); {
		case op < 5:
			stmt = fmt.Sprintf("INSERT INTO t VALUES (%d, %d, '%s')", id, u, n)
			wantErr = exists || uniqueTaken(u, -1)
			if !wantErr {
				model[id] = row{u, n}
			}
		case op < 8:
			stmt = fmt.Sprintf("UPDATE t SET u = %d, n = '%s' WHERE id = %d", u, n, id)
			wantErr = exists && uniqueTaken(u, id)
			if exists && !wantErr {
				model[id] = row{u, n}
			}
		case op < 9:
			stmt = fmt.Sprintf("DELETE FROM t WHERE id = %d", id)
			delete(model, id)
		default:
			_ = session.Close()
			if _, err := eng.Checkpoint(ctx, fmt.Sprint("step ", step)); err != nil {
				t.Fatalf("step %d checkpoint: %v", step, err)
			}
			continue
		}
		err := session.Exec(ctx, stmt)
		_ = session.Close()
		if (err != nil) != wantErr {
			t.Fatalf("step %d: %s: err = %v, want error %v", step, stmt, err, wantErr)
		}

		check := engines[rng.IntN(len(engines))]
		for probe := int64(0); probe < 60; probe += 7 {
			want := "[]"
			for id, r := range model {
				if r.u == probe {
					want = fmt.Sprintf("[[%d]]", id)
				}
			}
			if got := queryIDs(t, check, fmt.Sprintf("SELECT id FROM t WHERE u = %d", probe)); got != want {
				t.Fatalf("step %d after %s: u=%d -> %s, want %s", step, stmt, probe, got, want)
			}
		}
		for i := 0; i < 8; i++ {
			name := fmt.Sprint("n", i)
			var ids []int64
			for id, r := range model {
				if r.n == name {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			want := "["
			for j, id := range ids {
				if j > 0 {
					want += " "
				}
				want += fmt.Sprintf("[%d]", id)
			}
			want += "]"
			if got := queryIDs(t, check, fmt.Sprintf("SELECT id FROM t WHERE n = '%s' ORDER BY id", name)); got != want {
				t.Fatalf("step %d after %s: n=%s -> %s, want %s", step, stmt, name, got, want)
			}
		}
	}
}

// Index edits are layered: the journal's pending edits (shared) under the
// transaction's own. Unique checks, scans and statement rollback must see
// through both layers.
func TestJournalIndexOverlayLayers(t *testing.T) {
	ctx := context.Background()
	_, eng := openCheckpointedIndexedTable(t)
	// Pending, uncheckpointed rows: their index entries live only in the
	// shared journal overlay.
	execAll(t, eng,
		"INSERT INTO t VALUES (3, 30, 'p'), (4, 40, 'p')",
		"UPDATE t SET n = 'q' WHERE id = 1",
	)
	expectDuplicate(t, eng, "INSERT INTO t VALUES (5, 30, 'x')")

	session, _ := eng.NewSession()
	defer session.Close()
	tx, err := session.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ids := func(query string) string {
		t.Helper()
		result, err := tx.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(result.Rows)
	}
	// A failing multi-row statement adds index entries for its first row, then
	// hits a pending key; rollback of the statement must drop them.
	if err := tx.Exec(ctx, "INSERT INTO t VALUES (6, 60, 'p'), (7, 40, 'z')"); err == nil {
		t.Fatal("duplicate of a pending unique key was accepted")
	}
	if got := ids("SELECT id FROM t WHERE u = 60"); got != "[]" {
		t.Fatalf("rolled-back statement left an index entry: %s", got)
	}
	if err := tx.Exec(ctx, "INSERT INTO t VALUES (6, 60, 'p')"); err != nil {
		t.Fatalf("re-insert after statement rollback: %v", err)
	}
	// A local delete hides a key that exists only in the pending overlay, and
	// a local update moves another row into the scanned range.
	if err := tx.Exec(ctx, "DELETE FROM t WHERE id = 3"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "UPDATE t SET n = 'p' WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if got := ids("SELECT id FROM t WHERE n = 'p' ORDER BY id"); got != "[[2] [4] [6]]" {
		t.Fatalf("n = 'p' in transaction: %s", got)
	}
	if got := ids("SELECT id FROM t WHERE n = 'q'"); got != "[[1]]" {
		t.Fatalf("n = 'q' in transaction: %s", got)
	}
	// The deleted row's unique key is free again inside the transaction.
	if err := tx.Exec(ctx, "INSERT INTO t VALUES (8, 30, 'r')"); err != nil {
		t.Fatalf("reuse of a locally deleted unique key: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE n = 'p' ORDER BY id"); got != "[[2] [4] [6]]" {
		t.Fatalf("n = 'p' after commit: %s", got)
	}
	if got := queryIDs(t, eng, "SELECT id FROM t WHERE u = 30"); got != "[[8]]" {
		t.Fatalf("u = 30 after commit: %s", got)
	}
	// A fresh engine rebuilds the overlay from the journal.
	if got := queryIDs(t, openJournal(t, eng.Repository().Root), "SELECT id FROM t WHERE n = 'p' ORDER BY id"); got != "[[2] [4] [6]]" {
		t.Fatalf("n = 'p' after reopen: %s", got)
	}
}

// A unique value that moves to another row after a checkpoint belongs to its
// new row when an engine derives index edits from the journal, whatever the
// rows' key order (rdb-9afb3c).
func TestJournalDerivedUniqueIndexFollowsMovedValue(t *testing.T) {
	root, eng := openCheckpointedIndexedTable(t)
	defer eng.Close()
	execAll(t, eng, "DELETE FROM t WHERE id = 2", "UPDATE t SET u = 20 WHERE id = 1")
	fresh := openJournal(t, root)
	defer fresh.Close()
	if got := queryIDs(t, fresh, "SELECT id FROM t WHERE u = 20"); got != "[[1]]" {
		t.Fatalf("u=20 = %s, want [[1]]", got)
	}
	expectDuplicate(t, fresh, "INSERT INTO t VALUES (3, 20, 'c')")
}
