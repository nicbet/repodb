package engine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

type querier interface {
	Query(ctx context.Context, statement string, args ...any) (engine.Result, error)
}

// projectionFixture builds t (300 rows, a secondary index) and u. In journal
// mode it checkpoints and then leaves pending updates, deletes and inserts.
func projectionFixture(t *testing.T, mode engine.PersistenceMode) (*engine.Engine, *engine.Session) {
	t.Helper()
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	s, err := eng.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	exec := func(q string) {
		t.Helper()
		if err := s.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE TABLE t (id BIGINT PRIMARY KEY, a TEXT NOT NULL, b BIGINT NOT NULL, c TEXT, d DECIMAL(10,2), INDEX ib (b))")
	exec("CREATE TABLE u (id BIGINT PRIMARY KEY, name TEXT NOT NULL)")
	exec("INSERT INTO u VALUES (1, 'author')")
	var q strings.Builder
	q.WriteString("INSERT INTO t VALUES ")
	for id := 1; id <= 300; id++ {
		if id > 1 {
			q.WriteByte(',')
		}
		c := fmt.Sprintf("'c%d'", id)
		if id%7 == 0 {
			c = "NULL"
		}
		fmt.Fprintf(&q, "(%d, 'a%d', %d, %s, %d.25)", id, id, id%50, c, id)
	}
	exec(q.String())
	if mode == engine.PersistenceJournal {
		if _, err := eng.Checkpoint(ctx, "fixture"); err != nil {
			t.Fatal(err)
		}
		exec("UPDATE t SET a = 'pending', c = NULL WHERE id BETWEEN 60 AND 70")
		exec("DELETE FROM t WHERE id IN (3, 80, 81)")
		exec("INSERT INTO t VALUES (400, 'new', 20, 'n', 1.00)")
	}
	return eng, s
}

// Projected reads (fewer columns than the table) return the same values as
// full-row reads, through every access path and every layer of edits.
func TestProjectedReadsMatchFullRows(t *testing.T) {
	ctx := context.Background()
	queries := []struct {
		projected, reference string
	}{
		{"SELECT b FROM t", "SELECT * FROM t"},
		{"SELECT d, a FROM t WHERE id BETWEEN 50 AND 120", "SELECT * FROM t WHERE id BETWEEN 50 AND 120"},
		{"SELECT c FROM t WHERE b = 20", "SELECT * FROM t WHERE b = 20"},
		{"SELECT a, c FROM t WHERE id IN (3, 65, 77, 400)", "SELECT * FROM t WHERE id IN (3, 65, 77, 400)"},
	}
	// Column ordinals of each projected query, for projecting the reference rows.
	ordinals := [][]int{{2}, {4, 1}, {3}, {1, 3}}
	check := func(t *testing.T, label string, q querier) {
		t.Helper()
		for i, query := range queries {
			got, err := q.Query(ctx, query.projected)
			if err != nil {
				t.Fatalf("%s: %s: %v", label, query.projected, err)
			}
			ref, err := q.Query(ctx, query.reference)
			if err != nil {
				t.Fatal(err)
			}
			var want [][]any
			for _, row := range ref.Rows {
				projected := make([]any, len(ordinals[i]))
				for j, ordinal := range ordinals[i] {
					projected[j] = row[ordinal]
				}
				want = append(want, projected)
			}
			if len(want) == 0 {
				t.Fatalf("%s: %s: empty reference", label, query.reference)
			}
			if fmt.Sprint(got.Rows) != fmt.Sprint(want) {
				t.Fatalf("%s: %s = %v, want %v", label, query.projected, got.Rows, want)
			}
		}
		total, err := q.Query(ctx, "SELECT COUNT(*) FROM t")
		if err != nil {
			t.Fatal(err)
		}
		joined, err := q.Query(ctx, "SELECT u.name, COUNT(*) FROM t JOIN u ON u.id = 1 GROUP BY u.name")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(joined.Rows), fmt.Sprintf("[[author %v]]", total.Rows[0][0]); got != want {
			t.Fatalf("%s: join = %s, want %s", label, got, want)
		}
	}
	for _, mode := range []engine.PersistenceMode{engine.PersistenceNativeGit, engine.PersistenceJournal} {
		t.Run(string(mode), func(t *testing.T) {
			eng, s := projectionFixture(t, mode)
			if plan := explainPlan(t, eng, "SELECT b FROM t"); !strings.Contains(plan, "columns: [b]") {
				t.Fatalf("scan is not projected:\n%s", plan)
			}
			if plan := explainPlan(t, eng, "SELECT u.name, COUNT(*) FROM t JOIN u ON u.id = 1 GROUP BY u.name"); !strings.Contains(plan, "columns: []") {
				t.Fatalf("join scan is not projected to no columns:\n%s", plan)
			}
			check(t, "committed", s)
			tx, err := s.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, q := range []string{
				"UPDATE t SET d = 9.99, b = 20 WHERE id IN (65, 77)",
				"DELETE FROM t WHERE id = 100",
				"INSERT INTO t VALUES (401, 'tx', 20, NULL, NULL)",
			} {
				if err := tx.Exec(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			check(t, "in transaction", tx)
		})
	}
}

// COUNT(*) is answered from the tree's stored count when nothing is pending,
// and stays correct when the transaction or the journal holds edits.
func TestCountStarUsesRowCount(t *testing.T) {
	ctx := context.Background()
	count := func(q querier) string {
		t.Helper()
		result, err := q.Query(ctx, "SELECT COUNT(*) FROM t")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(result.Rows[0][0])
	}
	eng, s := projectionFixture(t, engine.PersistenceNativeGit)
	if plan := explainPlan(t, eng, "SELECT COUNT(*) FROM t"); !strings.Contains(plan, "table_count(t)") {
		t.Fatalf("COUNT(*) does not use the row count:\n%s", plan)
	}
	if got := count(s); got != "300" {
		t.Fatalf("COUNT(*) = %s, want 300", got)
	}
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "DELETE FROM t WHERE id <= 10"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO t VALUES (500, 'x', 1, NULL, NULL)"); err != nil {
		t.Fatal(err)
	}
	if got := count(tx); got != "291" {
		t.Fatalf("COUNT(*) in a transaction = %s, want 291", got)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := count(s); got != "291" {
		t.Fatalf("COUNT(*) after commit = %s, want 291", got)
	}

	_, js := projectionFixture(t, engine.PersistenceJournal)
	// 300 rows, 3 pending deletes and 1 pending insert.
	if got := count(js); got != "298" {
		t.Fatalf("COUNT(*) with pending journal edits = %s, want 298", got)
	}
}
