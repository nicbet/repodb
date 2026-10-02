package engine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

// BenchmarkSQLAutocommitScans measures range, ordered-limit (both directions),
// full-scan, join and count queries in their own implicit transactions over a
// checkpointed 50k-row journal table. The join is the scorecard's
// join_aggregate.
func BenchmarkSQLAutocommitScans(b *testing.B) {
	eng, repo := sqlBenchmarkEngineWithRepository(b, 50_000, 32, 1)
	if err := eng.Close(); err != nil {
		b.Fatal(err)
	}
	eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		b.Fatal(err)
	}
	defer eng.Close()
	setup, _ := eng.NewSession()
	for _, q := range []string{"CREATE TABLE authors (id BIGINT PRIMARY KEY, name TEXT NOT NULL)", "INSERT INTO authors VALUES (1, 'author')"} {
		if err := setup.Exec(context.Background(), q); err != nil {
			b.Fatal(err)
		}
	}
	setup.Close()
	if _, err := eng.Checkpoint(context.Background(), "authors"); err != nil {
		b.Fatal(err)
	}
	for _, q := range []struct{ name, sql string }{
		{"range100", "SELECT id, value FROM bench WHERE id BETWEEN 25000 AND 25099"},
		{"limit20", "SELECT id, value FROM bench ORDER BY id LIMIT 20"},
		{"desc-limit20", "SELECT id, value FROM bench ORDER BY id DESC LIMIT 20"},
		{"after-limit20", "SELECT id, value FROM bench WHERE id > 49000 ORDER BY id LIMIT 20"},
		{"range10k", "SELECT id, value FROM bench WHERE id BETWEEN 20000 AND 29999"},
		{"fullscan", "SELECT id, value FROM bench"},
		{"join", "SELECT a.name, COUNT(*) FROM bench b JOIN authors a ON a.id = 1 GROUP BY a.name"},
		{"count", "SELECT COUNT(*) FROM bench"},
	} {
		b.Run(q.name, func(b *testing.B) {
			session, _ := eng.NewSession()
			defer session.Close()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := session.Query(ctx, q.sql); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSQLAutocommitIndexScan measures a secondary-index range that fetches
// thousands of rows by primary key: the scorecard's issue_assignee_query over
// a checkpointed 100k-row journal table.
func BenchmarkSQLAutocommitIndexScan(b *testing.B) {
	const rows, assignees = 100_000, 20
	ctx := context.Background()
	root := gitRepository(b)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		b.Fatal(err)
	}
	setup, _ := eng.NewSession()
	if err := setup.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, status VARCHAR(16) NOT NULL, assignee VARCHAR(32), title VARCHAR(120) NOT NULL, body TEXT NOT NULL, INDEX by_assignee (assignee, status))"); err != nil {
		b.Fatal(err)
	}
	tx, err := setup.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	statuses := []string{"open", "in_progress", "review", "done"}
	body := strings.Repeat("b", 512)
	for start := 1; start <= rows; start += 500 {
		var query strings.Builder
		query.WriteString("INSERT INTO issues VALUES ")
		for id := start; id < start+500 && id <= rows; id++ {
			if id != start {
				query.WriteByte(',')
			}
			fmt.Fprintf(&query, "(%d,'%s','user%02d','Issue %d','%s')", id, statuses[id/assignees%len(statuses)], id%assignees, id, body)
		}
		if err := tx.Exec(ctx, query.String()); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}
	setup.Close()
	if err := eng.Close(); err != nil {
		b.Fatal(err)
	}
	eng, err = engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		b.Fatal(err)
	}
	defer eng.Close()
	session, _ := eng.NewSession()
	defer session.Close()
	want := rows / assignees * 3 / 4
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		result, err := session.Query(ctx, fmt.Sprintf("SELECT id, title, status FROM issues WHERE assignee = 'user%02d' AND status <> 'done'", i%assignees))
		if err != nil {
			b.Fatal(err)
		}
		if len(result.Rows) != want {
			b.Fatalf("rows = %d, want %d", len(result.Rows), want)
		}
	}
}
