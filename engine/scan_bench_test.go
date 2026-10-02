package engine_test

import (
	"context"
	"testing"

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
