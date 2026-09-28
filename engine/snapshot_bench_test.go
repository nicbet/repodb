package engine_test

import (
	"context"
	"testing"

	"github.com/nicbet/repodb/engine"
)

// BenchmarkSQLAutocommitPointRead measures a point read in its own implicit
// transaction, so every query resolves the current snapshot. This is the path
// that must notice other processes' writes.
func BenchmarkSQLAutocommitPointRead(b *testing.B) {
	for _, mode := range []string{"native-git", "journal-clean", "journal-dirty"} {
		b.Run(mode, func(b *testing.B) {
			ctx := context.Background()
			eng, repo := sqlBenchmarkEngineWithRepository(b, 10_000, 32, 1)
			if mode != "native-git" {
				if err := eng.Close(); err != nil {
					b.Fatal(err)
				}
				var err error
				eng, err = engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceJournal})
				if err != nil {
					b.Fatal(err)
				}
			}
			defer eng.Close()
			session, _ := eng.NewSession()
			defer session.Close()
			if mode == "journal-dirty" {
				if err := session.Exec(ctx, "UPDATE bench SET value = 'dirty' WHERE id = 1"); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, err := session.Query(ctx, "SELECT value FROM bench WHERE id = 5000")
				if err != nil || len(result.Rows) != 1 {
					b.Fatalf("point read = %v, %v", result, err)
				}
			}
		})
	}
}
