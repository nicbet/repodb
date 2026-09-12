package engine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func BenchmarkSQLTransactionBeginRollback(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			eng := sqlBenchmarkEngine(b, rows, 32, 1)
			defer eng.Close()
			session, _ := eng.NewSession()
			defer session.Close()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				tx, err := session.Begin(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if err := tx.Rollback(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLWarmReads(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			eng := sqlBenchmarkEngine(b, rows, 32, 1)
			defer eng.Close()
			for _, query := range []struct{ name, sql string }{
				{"point-hit", fmt.Sprintf("SELECT value FROM bench WHERE id = %d", rows/2)},
				{"point-miss", "SELECT value FROM bench WHERE id = -1"},
				{"limited-list", "SELECT id, value FROM bench ORDER BY id LIMIT 20"},
				{"full-scan", "SELECT id, value FROM bench"},
			} {
				b.Run(query.name, func(b *testing.B) {
					session, _ := eng.NewSession()
					defer session.Close()
					ctx := context.Background()
					tx, err := session.Begin(ctx)
					if err != nil {
						b.Fatal(err)
					}
					defer tx.Rollback(ctx)
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if _, err := tx.Query(ctx, query.sql); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func BenchmarkSQLWriteBatches(b *testing.B) {
	for _, batch := range []int{1, 10, 100, 1_000} {
		b.Run(fmt.Sprintf("mutations=%d", batch), func(b *testing.B) {
			eng := sqlBenchmarkEngine(b, 10_000, 32, 1)
			defer eng.Close()
			session, _ := eng.NewSession()
			defer session.Close()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				tx, err := session.Begin(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if err := tx.Exec(ctx, "UPDATE bench SET value = ? WHERE id >= 1 AND id <= ?", fmt.Sprintf("revision-%08d", i), batch); err != nil {
					b.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func sqlBenchmarkEngine(b *testing.B, rows, payloadBytes, tables int) *engine.Engine {
	b.Helper()
	ctx := context.Background()
	root := gitRepository(b)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	eng, err := engine.New(repo)
	if err != nil {
		b.Fatal(err)
	}
	session, _ := eng.NewSession()
	defer session.Close()
	for table := 0; table < tables; table++ {
		name := "bench"
		if table != 0 {
			name = fmt.Sprintf("bench_%03d", table)
		}
		if err := session.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id BIGINT PRIMARY KEY, value TEXT NOT NULL)", name)); err != nil {
			b.Fatal(err)
		}
		tx, err := session.Begin(ctx)
		if err != nil {
			b.Fatal(err)
		}
		const batchSize = 500
		payload := strings.Repeat("x", payloadBytes)
		for start := 1; start <= rows; start += batchSize {
			end := min(start+batchSize-1, rows)
			var query strings.Builder
			fmt.Fprintf(&query, "INSERT INTO %s VALUES ", name)
			for id := start; id <= end; id++ {
				if id != start {
					query.WriteByte(',')
				}
				fmt.Fprintf(&query, "(%d,'%s')", id, payload)
			}
			if err := tx.Exec(ctx, query.String()); err != nil {
				b.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			b.Fatal(err)
		}
	}
	return eng
}
