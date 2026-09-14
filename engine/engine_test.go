package engine_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func TestPersistentEmbeddedSQLCommitRollbackAndParameters(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eng.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title VARCHAR(200) NOT NULL, closed BOOLEAN NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO issues VALUES (?, ?, ?)", int64(1), "can't reproduce", false); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (?, ?, ?)", int64(2), "rolled back", true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = eng.Close()

	reopened, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader, _ := reopened.NewSession()
	defer reader.Close()
	result, err := reader.Query(ctx, "SELECT id, title, closed FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][1] != "can't reproduce" {
		t.Fatalf("rows = %#v", result.Rows)
	}
	status := exec.Command("git", "status", "--porcelain=v1")
	status.Dir = root
	if output, err := status.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("SQL changed source worktree: %q, %v", output, err)
	}
}

func TestStaleEmbeddedTransactionIsRejected(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	a, _ := eng.NewSession()
	b, _ := eng.NewSession()
	if err := a.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	ta, _ := a.Begin(ctx)
	tb, _ := b.Begin(ctx)
	if err := ta.Exec(ctx, "INSERT INTO issues VALUES (1, 'winner')"); err != nil {
		t.Fatal(err)
	}
	if err := tb.Exec(ctx, "INSERT INTO issues VALUES (2, 'stale')"); err != nil {
		t.Fatal(err)
	}
	if err := ta.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tb.Commit(ctx); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("stale commit = %v", err)
	}
	reader, _ := eng.NewSession()
	result, err := reader.Query(ctx, "SELECT title FROM issues")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "winner" {
		t.Fatalf("rows = %#v", result.Rows)
	}
}

func TestStatementFailureDoesNotAbortOrLeakPartialChanges(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (1, 'one')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (1, 'duplicate')"); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (2, 'two')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, title FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || result.Rows[0][1] != "one" || result.Rows[1][1] != "two" {
		t.Fatalf("rows = %#v", result.Rows)
	}
	snapshot, err := eng.Repository().Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Manifest.Objects) > 4 {
		t.Fatalf("live inventory retained obsolete chunks: %d objects", len(snapshot.Manifest.Objects))
	}
}

func TestDeletePublishesRowRemoval(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO issues VALUES (1, 'remove'), (2, 'keep')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "DELETE FROM issues WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, title FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(2) || result.Rows[0][1] != "keep" {
		t.Fatalf("rows = %#v", result.Rows)
	}
}

func TestDDLUsesMySQLImplicitCommit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "CREATE TABLE durable_ddl (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "SELECT * FROM durable_ddl"); err != nil {
		t.Fatalf("DDL was rolled back, want MySQL implicit commit: %v", err)
	}
}

func TestEmbeddedCommitOutcomeAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name               string
		point              repository.PublicationPoint
		outcome, recovered repository.CommitOutcome
	}{
		{"committed", repository.AfterRefPublication, repository.OutcomeCommitted, repository.OutcomeCommitted},
		{"unknown", repository.DuringRefPublication, repository.OutcomeUnknown, repository.OutcomeRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := gitRepository(t)
			repo, err := repository.Init(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			eng, _ := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceNativeGit})
			defer eng.Close()
			session, _ := eng.NewSession()
			if err := session.Exec(ctx, "CREATE TABLE outcomes (id BIGINT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			tx, _ := session.Begin(ctx)
			if err := tx.Exec(ctx, "INSERT INTO outcomes VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			repo.SetPublicationFaultInjector(func(point repository.PublicationPoint) error {
				if point == test.point {
					return errors.New("injected SQL commit fault")
				}
				return nil
			})
			err = tx.Commit(ctx)
			var commitErr *repository.CommitError
			if !errors.As(err, &commitErr) || commitErr.Outcome != test.outcome || commitErr.Commit == "" {
				t.Fatalf("commit error = %#v (%v)", commitErr, err)
			}
			repo.SetPublicationFaultInjector(nil)
			recovered, err := repo.RecoverCommit(ctx, commitErr.Commit)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Outcome != test.recovered {
				t.Fatalf("recovered = %v, want %v", recovered.Outcome, test.recovered)
			}
		})
	}
}

func TestMetadataCacheHitsOnRepeatedReads(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE items (id BIGINT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO items VALUES (1, 'cached')"); err != nil {
		t.Fatal(err)
	}
	engine.ResetPerformanceCounters()
	for i := 0; i < 5; i++ {
		result, err := s.Query(ctx, "SELECT value FROM items WHERE id = 1")
		if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "cached" {
			t.Fatalf("read %d: rows=%#v err=%v", i, result.Rows, err)
		}
	}
	counters := engine.ReadPerformanceCounters()
	if counters.MetadataCacheHits < 4 {
		t.Fatalf("expected at least 4 metadata cache hits, got %d hits / %d misses", counters.MetadataCacheHits, counters.MetadataCacheMisses)
	}
}

func gitRepository(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", "-b", "main")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return root
}
