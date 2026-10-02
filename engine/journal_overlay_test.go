package engine_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A journal commit appends only its own row edits, not the pending edits of
// earlier transactions, so its record size does not grow with them.
func TestJournalCommitAppendsOnlyOwnEdits(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	eng, s := openJournalEngine(t, ctx, root)
	path := eng.WorkingState().JournalPath()
	exec := func(q string) {
		t.Helper()
		if err := s.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	appended := func(q string) int64 {
		t.Helper()
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		exec(q)
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return after.Size() - before.Size()
	}
	exec("CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT NOT NULL)")
	exec("INSERT INTO t VALUES (1, 'a')")
	small := appended("UPDATE t SET v = 'b' WHERE id = 1")
	for start := 2; start <= 2001; start += 500 {
		var q strings.Builder
		q.WriteString("INSERT INTO t VALUES ")
		for id := start; id < start+500; id++ {
			if id != start {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "(%d, 'row %d')", id, id)
		}
		exec(q.String())
	}
	large := appended("UPDATE t SET v = 'c' WHERE id = 1")
	// The records differ only in generation and transaction ID digits.
	if large > small+16 {
		t.Fatalf("single-row update appended %d bytes with 2,000 pending edits, %d with none", large, small)
	}

	// A delete of a row that exists only in the pending overlay is written.
	exec("DELETE FROM t WHERE id = 5")
	exec("UPDATE t SET v = 'own' WHERE id = 7")

	verify := func(label string) {
		t.Helper()
		_, s := openJournalEngine(t, ctx, root)
		if got := countRows(t, ctx, s, "t"); got != 2000 {
			t.Fatalf("%s: %d rows, want 2000", label, got)
		}
		result, err := s.Query(ctx, "SELECT id, v FROM t WHERE id IN (1, 5, 7, 2001) ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(result.Rows), "[[1 c] [7 own] [2001 row 2001]]"; got != want {
			t.Fatalf("%s: rows = %s, want %s", label, got, want)
		}
	}
	verify("replayed")
	if _, err := eng.Checkpoint(ctx, "checkpoint"); err != nil {
		t.Fatal(err)
	}
	verify("checkpointed")
}
