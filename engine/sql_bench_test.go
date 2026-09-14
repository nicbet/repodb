package engine_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func BenchmarkM43DurableSave(b *testing.B) {
	for _, mode := range []engine.PersistenceMode{engine.PersistenceNativeGit, engine.PersistenceJournal} {
		for _, rows := range []int{1_000, 10_000, 50_000} {
			for _, batch := range []int{1, 10, 100} {
				b.Run(fmt.Sprintf("mode=%s/rows=%d/batch=%d", mode, rows, batch), func(b *testing.B) {
					eng, repo := sqlBenchmarkEngineWithRepository(b, rows, 64, 1)
					if mode == engine.PersistenceJournal {
						_ = eng.Close()
						var err error
						eng, err = engine.NewWithOptions(repo, engine.Options{Persistence: mode})
						if err != nil {
							b.Fatal(err)
						}
					}
					defer eng.Close()
					session, _ := eng.NewSession()
					defer session.Close()
					ctx := context.Background()
					beforeHead, _ := repo.Head(ctx)
					var beforeBytes int64
					if eng.WorkingState() != nil {
						if info, err := os.Stat(eng.WorkingState().JournalPath()); err == nil {
							beforeBytes = info.Size()
						}
					}
					engine.ResetPerformanceCounters()
					if eng.WorkingState() != nil {
						eng.WorkingState().ResetMetrics()
					}
					latencies := make([]time.Duration, 0, b.N)
					var beginNanos, sqlMutationNanos, commitNanos uint64
					b.ReportAllocs()
					b.ResetTimer()
					for i := range b.N {
						started := time.Now()
						phase := time.Now()
						tx, err := session.Begin(ctx)
						if err != nil {
							b.Fatal(err)
						}
						beginNanos += uint64(time.Since(phase))
						phase = time.Now()
						for offset := range batch {
							key := 1 + (i*batch+offset)%rows
							if err := tx.Exec(ctx, "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("m43-%08d-%03d", i, offset), key); err != nil {
								b.Fatal(err)
							}
						}
						sqlMutationNanos += uint64(time.Since(phase))
						phase = time.Now()
						if err := tx.Commit(ctx); err != nil {
							b.Fatal(err)
						}
						commitNanos += uint64(time.Since(phase))
						latencies = append(latencies, time.Since(started))
					}
					b.StopTimer()
					afterHead, _ := repo.Head(ctx)
					if mode == engine.PersistenceJournal && afterHead != beforeHead {
						b.Fatalf("journal advanced Git head: %s -> %s", beforeHead, afterHead)
					}
					if eng.WorkingState() != nil {
						info, err := os.Stat(eng.WorkingState().JournalPath())
						if err != nil {
							b.Fatal(err)
						}
						b.ReportMetric(float64(info.Size()-beforeBytes)/float64(b.N), "journal-bytes/op")
						metrics := eng.WorkingState().Metrics()
						b.ReportMetric(float64(metrics.LoadNanos)/float64(b.N), "prior-state-ns/op")
						b.ReportMetric(float64(metrics.LoadBytes)/float64(b.N), "replay-bytes/op")
						b.ReportMetric(float64(metrics.LoadFrames)/float64(b.N), "replay-frames/op")
						b.ReportMetric(float64(metrics.PrepareNanos)/float64(b.N), "object-copy-ns/op")
						b.ReportMetric(float64(metrics.PreparedBytes)/float64(b.N), "object-bytes/op")
						b.ReportMetric(float64(metrics.EncodeNanos)/float64(b.N), "encode-ns/op")
						b.ReportMetric(float64(metrics.AppendNanos)/float64(b.N), "append-ns/op")
						b.ReportMetric(float64(metrics.FlushNanos)/float64(b.N), "flush-ns/op")
					}
					counters := engine.ReadPerformanceCounters()
					b.ReportMetric(float64(beginNanos)/float64(b.N), "begin-ns/op")
					b.ReportMetric(float64(sqlMutationNanos)/float64(b.N), "sql-mutation-ns/op")
					b.ReportMetric(float64(commitNanos)/float64(b.N), "commit-ns/op")
					b.ReportMetric(float64(counters.SnapshotBuildNanos)/float64(b.N), "snapshot-build-ns/op")
					b.ReportMetric(float64(counters.TreeMutationNanos)/float64(b.N), "tree-mutation-ns/op")
					b.ReportMetric(float64(counters.ReachabilityNanos)/float64(b.N), "reachability-ns/op")
					sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
					if len(latencies) > 0 {
						b.ReportMetric(float64(latencies[len(latencies)/2]), "request-p50-ns")
						b.ReportMetric(float64(latencies[(len(latencies)*95-1)/100]), "request-p95-ns")
						b.ReportMetric(float64(latencies[len(latencies)-1]), "request-max-ns")
					}
				})
			}
		}
	}
}

func BenchmarkM43JournalCheckpoint(b *testing.B) {
	for _, batch := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			b.StopTimer()
			for range b.N {
				eng, repo := sqlBenchmarkEngineWithRepository(b, 10_000, 64, 1)
				_ = eng.Close()
				eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceJournal})
				if err != nil {
					b.Fatal(err)
				}
				session, _ := eng.NewSession()
				tx, _ := session.Begin(context.Background())
				for key := 1; key <= batch; key++ {
					if err := tx.Exec(context.Background(), "UPDATE bench SET value = ? WHERE id = ?", "checkpoint", key); err != nil {
						b.Fatal(err)
					}
				}
				if err := tx.Commit(context.Background()); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := eng.WorkingState().Checkpoint(context.Background(), "M4.3 benchmark checkpoint"); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				_ = eng.Close()
			}
		})
	}
}

func BenchmarkM43JournalReplayGrowth(b *testing.B) {
	for _, generations := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("generations=%d", generations), func(b *testing.B) {
			eng, repo := sqlBenchmarkEngineWithRepository(b, 1_000, 64, 1)
			_ = eng.Close()
			eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceJournal})
			if err != nil {
				b.Fatal(err)
			}
			session, _ := eng.NewSession()
			for generation := range generations {
				if err := session.Exec(context.Background(), "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("growth-%08d", generation), 1+generation%1_000); err != nil {
					b.Fatal(err)
				}
			}
			working := eng.WorkingState()
			want, err := working.Current(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			b.Run("full-replay", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					fresh, err := repository.OpenWorkingState(repo)
					if err != nil {
						b.Fatal(err)
					}
					snapshot, err := fresh.Current(context.Background())
					if err != nil || snapshot.Generation() != want.Generation() {
						b.Fatalf("full replay generation = %d, %v", snapshot.Generation(), err)
					}
				}
			})
			b.Run("cached", func(b *testing.B) {
				working.ResetMetrics()
				b.ReportAllocs()
				for range b.N {
					snapshot, err := working.Current(context.Background())
					if err != nil || snapshot.Generation() != want.Generation() {
						b.Fatalf("cached load generation = %d, %v", snapshot.Generation(), err)
					}
				}
				metrics := working.Metrics()
				b.ReportMetric(float64(metrics.LoadBytes)/float64(b.N), "replay-bytes/op")
				b.ReportMetric(float64(metrics.LoadFrames)/float64(b.N), "replay-frames/op")
			})
			_ = eng.Close()
		})
	}
}

func BenchmarkM43JournalFirstSaveAfterCheckpoint(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			b.StopTimer()
			eng, repo := sqlBenchmarkEngineWithRepository(b, rows, 64, 1)
			_ = eng.Close()
			eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceJournal})
			if err != nil {
				b.Fatal(err)
			}
			defer eng.Close()
			session, _ := eng.NewSession()
			latencies := make([]time.Duration, 0, b.N)
			b.ReportAllocs()
			for iteration := range b.N {
				if iteration > 0 {
					if _, err := eng.Checkpoint(context.Background(), "reset clean benchmark state"); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				started := time.Now()
				if err := session.Exec(context.Background(), "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("first-%08d", iteration), 1+iteration%rows); err != nil {
					b.Fatal(err)
				}
				latencies = append(latencies, time.Since(started))
				b.StopTimer()
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			if len(latencies) > 0 {
				b.ReportMetric(float64(latencies[len(latencies)/2]), "request-p50-ns")
				b.ReportMetric(float64(latencies[(len(latencies)*95-1)/100]), "request-p95-ns")
				b.ReportMetric(float64(latencies[len(latencies)-1]), "request-max-ns")
			}
		})
	}
}

type durabilitySample struct {
	metrics    repodbgit.DurabilityMetrics
	blobInputs uint64
}

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

func BenchmarkSQLExactKeyUpdate(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			eng := sqlBenchmarkEngine(b, rows, 32, 1)
			defer eng.Close()
			session, _ := eng.NewSession()
			defer session.Close()
			ctx := context.Background()
			engine.ResetPerformanceCounters()
			repository.ResetPublicationMetrics()
			repodbgit.ResetObjectWriteCount()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if err := session.Exec(ctx, "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("exact-%08d", i), rows/2); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			engineMetrics := engine.ReadPerformanceCounters()
			publication := repository.ReadPublicationMetrics()
			perOp := float64(b.N)
			b.ReportMetric(float64(engineMetrics.RowsDecoded)/perOp, "rows-decoded/op")
			b.ReportMetric(float64(engineMetrics.PointKeysVisited)/perOp, "point-keys/op")
			b.ReportMetric(float64(engineMetrics.TreeMutationNanos)/perOp, "tree-ns/op")
			b.ReportMetric(float64(engineMetrics.ReachabilityNanos)/perOp, "inventory-walk-ns/op")
			b.ReportMetric(float64(publication.InventoryNanos)/perOp, "publication-inventory-ns/op")
			b.ReportMetric(float64(publication.ObjectWriteNanos)/perOp, "object-write-ns/op")
			b.ReportMetric(float64(publication.TreeCommitNanos)/perOp, "tree-commit-ns/op")
			b.ReportMetric(float64(publication.RefUpdateNanos)/perOp, "ref-update-ns/op")
			b.ReportMetric(float64(publication.VerificationNanos)/perOp, "verification-ns/op")
			b.ReportMetric(float64(repodbgit.ObjectWriteCount())/perOp, "object-writes/op")
		})
	}
}

func BenchmarkGitDurabilityStrategies(b *testing.B) {
	type mutation struct {
		name string
		run  func(context.Context, *engine.Session, int) (int64, *string, error)
	}
	update := func(key int64, value func(int) string) func(context.Context, *engine.Session, int) (int64, *string, error) {
		return func(ctx context.Context, session *engine.Session, iteration int) (int64, *string, error) {
			expected := value(iteration)
			return key, &expected, session.Exec(ctx, "UPDATE bench SET value = ? WHERE id = ?", expected, key)
		}
	}
	mutations := []mutation{
		{"update-first-short", update(1, func(i int) string { return fmt.Sprintf("a%d", i) })},
		{"update-quarter-same", update(12_500, func(i int) string { return fmt.Sprintf("%032d", i) })},
		{"update-middle-long", update(25_000, func(i int) string { return strings.Repeat("m", 4_096) + fmt.Sprint(i) })},
		{"update-three-quarter-same", update(37_500, func(i int) string { return fmt.Sprintf("q%031d", i) })},
		{"update-last-short", update(50_000, func(i int) string { return fmt.Sprintf("z%d", i) })},
		{"insert-short", func(ctx context.Context, session *engine.Session, i int) (int64, *string, error) {
			key, expected := int64(1_000_000+i), fmt.Sprintf("i%d", i)
			return key, &expected, session.Exec(ctx, "INSERT INTO bench VALUES (?, ?)", key, expected)
		}},
		{"insert-long", func(ctx context.Context, session *engine.Session, i int) (int64, *string, error) {
			key, expected := int64(2_000_000+i), strings.Repeat("i", 4_096)+fmt.Sprint(i)
			return key, &expected, session.Exec(ctx, "INSERT INTO bench VALUES (?, ?)", key, expected)
		}},
		{"delete-early", func(ctx context.Context, session *engine.Session, i int) (int64, *string, error) {
			key := int64(5_000 + i)
			return key, nil, session.Exec(ctx, "DELETE FROM bench WHERE id = ?", key)
		}},
		{"delete-middle", func(ctx context.Context, session *engine.Session, i int) (int64, *string, error) {
			key := int64(25_001 + i)
			return key, nil, session.Exec(ctx, "DELETE FROM bench WHERE id = ?", key)
		}},
		{"delete-late", func(ctx context.Context, session *engine.Session, i int) (int64, *string, error) {
			key := int64(45_000 + i)
			return key, nil, session.Exec(ctx, "DELETE FROM bench WHERE id = ?", key)
		}},
	}
	seedEngine, seedRepo := sqlBenchmarkEngineWithRepository(b, 50_000, 32, 1)
	seedHead, err := seedRepo.Head(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	seedRoot := seedRepo.Root
	seedEngine.Close()
	for _, method := range []repodbgit.FsyncMethod{repodbgit.FsyncMethodFsync, repodbgit.FsyncMethodBatch} {
		b.Run(string(method), func(b *testing.B) {
			root := b.TempDir()
			cmd := exec.Command("cp", "-R", seedRoot+"/.", root)
			if output, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("copy benchmark seed: %v: %s", err, output)
			}
			repo, err := repository.Open(context.Background(), root)
			if err != nil {
				b.Fatal(err)
			}
			clonedHead, err := repo.Head(context.Background())
			if err != nil || clonedHead != seedHead {
				b.Fatalf("cloned head = %q, want %q: %v", clonedHead, seedHead, err)
			}
			eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceNativeGit})
			if err != nil {
				b.Fatal(err)
			}
			defer eng.Close()
			session, _ := eng.NewSession()
			defer session.Close()
			for _, change := range mutations {
				nextIteration := 0
				b.Run(change.name, func(b *testing.B) {
					ctx, audit, err := repodbgit.WithDurabilityAudit(context.Background(), repo.CommonDir, method)
					if err != nil {
						b.Fatal(err)
					}
					repodbgit.ResetObjectWriteCount()
					repodbgit.ResetObjectWriteBytes()
					b.ReportAllocs()
					samples := make([]durabilitySample, 0, b.N)
					b.ResetTimer()
					for i := range b.N {
						b.StopTimer()
						before, err := repo.Head(ctx)
						if err != nil {
							b.Fatal(err)
						}
						beforeAudit := audit.Metrics()
						beforeInputs := repodbgit.ObjectWriteCount()
						b.StartTimer()
						key, expected, err := change.run(ctx, session, nextIteration+i)
						b.StopTimer()
						samples = append(samples, durabilitySample{
							metrics:    subtractDurabilityMetrics(audit.Metrics(), beforeAudit),
							blobInputs: repodbgit.ObjectWriteCount() - beforeInputs,
						})
						if err != nil {
							b.Fatal(err)
						}
						after, err := repo.Head(ctx)
						if err != nil || after == before {
							b.Fatalf("data head did not advance: before=%q after=%q err=%v", before, after, err)
						}
						result, err := session.Query(context.Background(), "SELECT value FROM bench WHERE id = ?", key)
						if err != nil {
							b.Fatal(err)
						}
						if expected == nil {
							if len(result.Rows) != 0 {
								b.Fatalf("deleted key %d remains: %#v", key, result.Rows)
							}
						} else if len(result.Rows) != 1 || result.Rows[0][0] != *expected {
							b.Fatalf("key %d value = %#v, want %q", key, result.Rows, *expected)
						}
					}
					nextIteration += b.N
					b.StopTimer()
					perOp := float64(b.N)
					metrics := audit.Metrics()
					reportDurabilityCommand(b, "hash", metrics.HashObject, perOp)
					reportDurabilityCommand(b, "tree", metrics.WriteTree, perOp)
					reportDurabilityCommand(b, "commit", metrics.CommitTree, perOp)
					reportDurabilityCommand(b, "ref", metrics.UpdateRef, perOp)
					b.ReportMetric(float64(repodbgit.ObjectWriteCount())/perOp, "blob-inputs/op")
					b.ReportMetric(float64(repodbgit.ObjectWriteBytes())/perOp, "blob-input-bytes/op")
					reportDurabilityDistributions(b, samples)
				})
			}
		})
	}
}

func subtractDurabilityMetrics(after, before repodbgit.DurabilityMetrics) repodbgit.DurabilityMetrics {
	return repodbgit.DurabilityMetrics{
		HashObject: subtractDurabilityCommand(after.HashObject, before.HashObject),
		WriteTree:  subtractDurabilityCommand(after.WriteTree, before.WriteTree),
		CommitTree: subtractDurabilityCommand(after.CommitTree, before.CommitTree),
		UpdateRef:  subtractDurabilityCommand(after.UpdateRef, before.UpdateRef),
	}
}

func subtractDurabilityCommand(after, before repodbgit.DurabilityCommandMetrics) repodbgit.DurabilityCommandMetrics {
	return repodbgit.DurabilityCommandMetrics{
		Invocations: after.Invocations - before.Invocations, LatencyNanos: after.LatencyNanos - before.LatencyNanos,
		ObjectsCreated: after.ObjectsCreated - before.ObjectsCreated, ObjectBytes: after.ObjectBytes - before.ObjectBytes,
		HardwareFlushes: after.HardwareFlushes - before.HardwareFlushes, WriteoutRequests: after.WriteoutRequests - before.WriteoutRequests,
	}
}

func reportDurabilityDistributions(b *testing.B, samples []durabilitySample) {
	b.Helper()
	values := func(selectValue func(repodbgit.DurabilityMetrics, uint64) float64) []float64 {
		result := make([]float64, len(samples))
		for i, sample := range samples {
			result[i] = selectValue(sample.metrics, sample.blobInputs)
		}
		sort.Float64s(result)
		return result
	}
	report := func(name string, distribution []float64) {
		if len(distribution) == 0 {
			return
		}
		b.ReportMetric(distribution[0], name+"-min/op")
		b.ReportMetric(distribution[len(distribution)/2], name+"-p50/op")
		b.ReportMetric(distribution[len(distribution)-1], name+"-max/op")
	}
	report("blobs", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 { return float64(m.HashObject.ObjectsCreated) }))
	report("trees", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 { return float64(m.WriteTree.ObjectsCreated) }))
	report("reused", values(func(m repodbgit.DurabilityMetrics, inputs uint64) float64 {
		return float64(inputs - m.HashObject.ObjectsCreated)
	}))
	report("full-flushes", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 {
		return float64(m.HashObject.HardwareFlushes + m.WriteTree.HardwareFlushes + m.CommitTree.HardwareFlushes + m.UpdateRef.HardwareFlushes)
	}))
	report("writeouts", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 {
		return float64(m.HashObject.WriteoutRequests + m.WriteTree.WriteoutRequests + m.CommitTree.WriteoutRequests + m.UpdateRef.WriteoutRequests)
	}))
	report("hash-ms", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 { return float64(m.HashObject.LatencyNanos) / 1e6 }))
	report("tree-ms", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 { return float64(m.WriteTree.LatencyNanos) / 1e6 }))
	report("commit-ms", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 { return float64(m.CommitTree.LatencyNanos) / 1e6 }))
	report("ref-ms", values(func(m repodbgit.DurabilityMetrics, _ uint64) float64 { return float64(m.UpdateRef.LatencyNanos) / 1e6 }))
}

func reportDurabilityCommand(b *testing.B, name string, metric repodbgit.DurabilityCommandMetrics, perOp float64) {
	b.Helper()
	b.ReportMetric(float64(metric.LatencyNanos)/perOp, name+"-ns/op")
	b.ReportMetric(float64(metric.ObjectsCreated)/perOp, name+"-objects/op")
	b.ReportMetric(float64(metric.ObjectBytes)/perOp, name+"-disk-bytes/op")
	b.ReportMetric(float64(metric.HardwareFlushes)/perOp, name+"-full-flushes/op")
	b.ReportMetric(float64(metric.WriteoutRequests)/perOp, name+"-writeouts/op")
}

func sqlBenchmarkEngine(b *testing.B, rows, payloadBytes, tables int) *engine.Engine {
	eng, _ := sqlBenchmarkEngineWithRepository(b, rows, payloadBytes, tables)
	return eng
}

func sqlBenchmarkEngineWithRepository(b *testing.B, rows, payloadBytes, tables int) (*engine.Engine, *repository.Repository) {
	b.Helper()
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
	return eng, repo
}
