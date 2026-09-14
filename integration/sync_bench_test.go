package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
	"golang.org/x/sys/unix"
)

type syncBenchmarkFixture struct {
	remote       string
	local        string
	localHead    string
	remoteHead   string
	conflict     bool
	conflictTake engine.Resolution
	retry        bool
}

func BenchmarkSyncUpToDateWarm(b *testing.B) {
	fixture := newSyncBenchmarkFixture(b, 1_000, 1, "unchanged")
	ctx := context.Background()
	b.ReportAllocs()
	repodbgit.ResetProcessCount()
	repodbgit.ResetObjectWriteCount()
	b.ResetTimer()
	for range b.N {
		status, err := integration.Sync(ctx, fixture.local, "origin")
		if err != nil || status.Action != "up-to-date" {
			b.Fatalf("sync = %#v, %v", status, err)
		}
	}
	b.StopTimer()
	reportSyncMetrics(b)
}

func BenchmarkSyncDivergent(b *testing.B) {
	for _, test := range []struct {
		name, workload string
		rows, tables   int
	}{
		{"sparse-1000", "sparse", 1_000, 1},
		{"sparse-10000", "sparse", 10_000, 1},
		{"sparse-50000", "sparse", 50_000, 1},
		{"remote-selected", "remote-selected", 1_000, 1},
		{"many-unchanged", "sparse", 1_000, 50},
		{"conflict-resolution", "conflict", 1_000, 1},
		{"rejected-push-retry", "retry", 1_000, 1},
	} {
		b.Run(test.name, func(b *testing.B) {
			fixture := newSyncBenchmarkFixture(b, test.rows, test.tables, test.workload)
			ctx := context.Background()
			var growth int64
			b.ReportAllocs()
			repodbgit.ResetProcessCount()
			repodbgit.ResetObjectWriteCount()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				resetSyncBenchmark(b, fixture)
				before := directorySize(b, filepath.Join(fixture.local, ".git"))
				b.StartTimer()
				status, err := integration.Sync(ctx, fixture.local, "origin")
				if fixture.conflict {
					var conflictErr *integration.MergeConflictError
					if !errors.As(err, &conflictErr) || len(conflictErr.Set.Unresolved()) == 0 {
						b.Fatalf("conflict sync = %#v, %v", status, err)
					}
					for _, item := range conflictErr.Set.Unresolved() {
						status, err = integration.Resolve(ctx, fixture.local, "origin", item.ID, fixture.conflictTake)
						if err != nil {
							b.Fatal(err)
						}
					}
				}
				if err != nil || (status.Action != "merged" && !(fixture.retry && status.Action == "pushed")) {
					b.Fatalf("sync = %#v, %v", status, err)
				}
				b.StopTimer()
				growth += directorySize(b, filepath.Join(fixture.local, ".git")) - before
				b.StartTimer()
			}
			b.StopTimer()
			reportSyncMetrics(b)
			b.ReportMetric(float64(growth)/float64(b.N), "repo-bytes/op")
		})
	}
}

func reportSyncMetrics(b *testing.B) {
	b.ReportMetric(float64(repodbgit.ProcessCount())/float64(b.N), "git-procs/op")
	b.ReportMetric(float64(repodbgit.ObjectWriteCount())/float64(b.N), "object-writes/op")
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err == nil {
		peak := int64(usage.Maxrss)
		if runtime.GOOS != "darwin" {
			peak *= 1024
		}
		b.ReportMetric(float64(peak), "peak-rss-bytes")
	}
}

func newSyncBenchmarkFixture(b *testing.B, rows, tables int, workload string) syncBenchmarkFixture {
	b.Helper()
	ctx := context.Background()
	root := b.TempDir()
	remote := filepath.Join(root, "remote.git")
	a := filepath.Join(root, "a")
	local := filepath.Join(root, "b")
	benchGit(b, root, "init", "--bare", "--quiet", remote)
	for _, clone := range []string{a, local} {
		if err := os.MkdirAll(clone, 0o755); err != nil {
			b.Fatal(err)
		}
		benchGit(b, clone, "init", "--quiet", "-b", "main")
		benchGit(b, clone, "remote", "add", "origin", remote)
	}
	if _, err := integration.Enable(ctx, a, "origin"); err != nil {
		b.Fatal(err)
	}
	engA, err := engine.OpenWithOptions(ctx, a, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		b.Fatal(err)
	}
	sa, _ := engA.NewSession()
	if err := sa.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title VARCHAR(200) NOT NULL)"); err != nil {
		b.Fatal(err)
	}
	insertBenchmarkRows(b, ctx, sa, rows)
	for i := 1; i < tables; i++ {
		if err := sa.Exec(ctx, fmt.Sprintf("CREATE TABLE stable_%03d (id BIGINT PRIMARY KEY, value TEXT NOT NULL)", i)); err != nil {
			b.Fatal(err)
		}
		if err := sa.Exec(ctx, fmt.Sprintf("INSERT INTO stable_%03d VALUES (1, 'stable')", i)); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		b.Fatal(err)
	}
	if _, err := integration.Enable(ctx, local, "origin"); err != nil {
		b.Fatal(err)
	}
	if workload == "unchanged" {
		_ = sa.Close()
		_ = engA.Close()
		return syncBenchmarkFixture{remote: remote, local: local}
	}
	engB, err := engine.OpenWithOptions(ctx, local, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		b.Fatal(err)
	}
	sb, _ := engB.NewSession()
	switch workload {
	case "sparse", "retry":
		updateSyncBenchmarkRows(b, ctx, sb, rows, 0, "local")
	case "remote-selected":
		if err := sb.Exec(ctx, "INSERT INTO issues VALUES (1000001, 'local')"); err != nil {
			b.Fatal(err)
		}
	case "conflict":
		if err := sb.Exec(ctx, "UPDATE issues SET title = 'local' WHERE id = 1"); err != nil {
			b.Fatal(err)
		}
	}
	localHead := benchGit(b, local, "rev-parse", repository.DataRef)
	switch workload {
	case "sparse", "retry":
		updateSyncBenchmarkRows(b, ctx, sa, rows, 1, "remote")
	case "remote-selected":
		if err := sa.Exec(ctx, "CREATE TABLE remote_only (id BIGINT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
			b.Fatal(err)
		}
		if err := sa.Exec(ctx, "INSERT INTO remote_only VALUES (1, 'remote')"); err != nil {
			b.Fatal(err)
		}
	case "conflict":
		if err := sa.Exec(ctx, "UPDATE issues SET title = 'remote' WHERE id = 1"); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		b.Fatal(err)
	}
	remoteHead := benchGit(b, a, "rev-parse", repository.DataRef)
	_ = sb.Close()
	_ = engB.Close()
	_ = sa.Close()
	_ = engA.Close()
	fixture := syncBenchmarkFixture{remote: remote, local: local, localHead: localHead, remoteHead: remoteHead, conflict: workload == "conflict", conflictTake: engine.TakeRemote, retry: workload == "retry"}
	if fixture.retry {
		hook := filepath.Join(remote, "hooks", "pre-receive")
		script := "#!/bin/sh\nmarker=\"$(dirname \"$0\")/../repodb-reject-once\"\nif [ ! -e \"$marker\" ]; then\n  : >\"$marker\"\n  exit 1\nfi\n"
		if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
			b.Fatal(err)
		}
	}
	return fixture
}

func insertBenchmarkRows(b *testing.B, ctx context.Context, session *engine.Session, rows int) {
	b.Helper()
	tx, err := session.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	const batch = 500
	for start := 1; start <= rows; start += batch {
		end := min(start+batch-1, rows)
		var query strings.Builder
		query.WriteString("INSERT INTO issues VALUES ")
		for id := start; id <= end; id++ {
			if id != start {
				query.WriteByte(',')
			}
			fmt.Fprintf(&query, "(%d,'issue-%08d')", id, id)
		}
		if err := tx.Exec(ctx, query.String()); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}
}

func updateSyncBenchmarkRows(b *testing.B, ctx context.Context, session *engine.Session, rows, offset int, suffix string) {
	b.Helper()
	tx, err := session.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	for id := 1 + offset; id <= rows; id += 100 {
		if err := tx.Exec(ctx, "UPDATE issues SET title = ? WHERE id = ?", fmt.Sprintf("issue-%08d-%s", id, suffix), int64(id)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}
}

func resetSyncBenchmark(b *testing.B, fixture syncBenchmarkFixture) {
	b.Helper()
	benchGit(b, fixture.local, "update-ref", repository.DataRef, fixture.localHead)
	benchGit(b, fixture.remote, "update-ref", repository.DataRef, fixture.remoteHead)
	if fixture.retry {
		if err := os.Remove(filepath.Join(fixture.remote, "repodb-reject-once")); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.Fatal(err)
		}
	}
}

func directorySize(b *testing.B, root string) int64 {
	b.Helper()
	var size int64
	if err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return err
	}); err != nil {
		b.Fatal(err)
	}
	return size
}

func benchGit(b *testing.B, dir string, args ...string) string {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
