package engine_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

type mergeFixture struct {
	root                string
	repo                *repository.Repository
	base, local, remote *repository.Snapshot
}

var mergeFixtures sync.Map

func TestMain(m *testing.M) {
	code := m.Run()
	mergeFixtures.Range(func(_, value any) bool {
		_ = os.RemoveAll(value.(*mergeFixture).root)
		return true
	})
	os.Exit(code)
}

func benchmarkMergeFixture(b *testing.B, rows int) *mergeFixture {
	b.Helper()
	if value, ok := mergeFixtures.Load(rows); ok {
		return value.(*mergeFixture)
	}
	ctx := context.Background()
	root, err := os.MkdirTemp("", fmt.Sprintf("repodb-m4-%d-*", rows))
	if err != nil {
		b.Fatal(err)
	}
	runBenchmarkGit(b, root, "init", "--quiet", "-b", "main")
	repo, err := repository.Init(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		b.Fatal(err)
	}
	session, err := eng.NewSession()
	if err != nil {
		b.Fatal(err)
	}
	if err := session.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title VARCHAR(200) NOT NULL, closed BOOLEAN NOT NULL)"); err != nil {
		b.Fatal(err)
	}
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
			fmt.Fprintf(&query, "(%d,'issue-%08d',FALSE)", id, id)
		}
		if err := tx.Exec(ctx, query.String()); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}
	base, err := repo.Current(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if err := updateBenchmarkRows(ctx, session, rows, 0, "local"); err != nil {
		b.Fatal(err)
	}
	local, err := repo.Current(ctx)
	if err != nil {
		b.Fatal(err)
	}
	_ = session.Close()
	_ = eng.Close()

	runBenchmarkGit(b, root, "update-ref", repository.DataRef, base.Commit, local.Commit)
	eng, err = engine.New(repo)
	if err != nil {
		b.Fatal(err)
	}
	session, err = eng.NewSession()
	if err != nil {
		b.Fatal(err)
	}
	if err := updateBenchmarkRows(ctx, session, rows, 1, "remote"); err != nil {
		b.Fatal(err)
	}
	remote, err := repo.Current(ctx)
	if err != nil {
		b.Fatal(err)
	}
	_ = session.Close()
	_ = eng.Close()
	runBenchmarkGit(b, root, "update-ref", repository.DataRef, local.Commit, remote.Commit)
	for _, snapshot := range []*repository.Snapshot{base, local, remote} {
		if err := engine.ValidateSnapshot(ctx, snapshot); err != nil {
			b.Fatal(err)
		}
	}

	actual, loaded := mergeFixtures.LoadOrStore(rows, &mergeFixture{root: root, repo: repo, base: base, local: local, remote: remote})
	if loaded {
		_ = os.RemoveAll(root)
	}
	return actual.(*mergeFixture)
}

func updateBenchmarkRows(ctx context.Context, session *engine.Session, rows, offset int, suffix string) error {
	tx, err := session.Begin(ctx)
	if err != nil {
		return err
	}
	step := 100
	if rows < step {
		step = max(rows, 1)
	}
	for id := 1 + offset; id <= rows; id += step {
		if err := tx.Exec(ctx, "UPDATE issues SET title = ? WHERE id = ?", fmt.Sprintf("issue-%08d-%s", id, suffix), int64(id)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func runBenchmarkGit(b *testing.B, dir string, args ...string) {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func BenchmarkMergeSnapshotsSparseChanges(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			fixture := benchmarkMergeFixture(b, rows)
			ctx := context.Background()
			b.ReportAllocs()
			repodbgit.ResetProcessCount()
			b.ResetTimer()
			for range b.N {
				writer, err := fixture.repo.BeginMergeSnapshots(fixture.local, fixture.remote)
				if err != nil {
					b.Fatal(err)
				}
				_, _, conflicts, err := engine.MergeSnapshots(ctx, writer, fixture.base, fixture.local, fixture.remote, nil)
				if err != nil || len(conflicts) != 0 {
					b.Fatalf("merge: %v, conflicts=%d", err, len(conflicts))
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(repodbgit.ProcessCount())/float64(b.N), "git-procs/op")
		})
	}
}

func BenchmarkMergeSnapshotsUnchangedTable(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			fixture := benchmarkMergeFixture(b, rows)
			ctx := context.Background()
			b.ReportAllocs()
			repodbgit.ResetProcessCount()
			b.ResetTimer()
			for range b.N {
				writer, err := fixture.repo.BeginMergeSnapshots(fixture.local, fixture.remote)
				if err != nil {
					b.Fatal(err)
				}
				_, _, conflicts, err := engine.MergeSnapshots(ctx, writer, fixture.base, fixture.base, fixture.base, nil)
				if err != nil || len(conflicts) != 0 {
					b.Fatalf("merge: %v, conflicts=%d", err, len(conflicts))
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(repodbgit.ProcessCount())/float64(b.N), "git-procs/op")
		})
	}
}
