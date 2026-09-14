package engine_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func TestJournalSQLSurvivesRestartWithoutAdvancingGit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	if err := session.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO issues VALUES (1, 'durable working row')"); err != nil {
		t.Fatal(err)
	}
	if after, err := repo.Head(ctx); err != nil || after != before {
		t.Fatalf("journal save advanced Git head: before=%s after=%s err=%v", before, after, err)
	}
	status, err := eng.WorkingState().Status(ctx)
	if err != nil || !status.Dirty || status.Generation != 2 {
		t.Fatalf("working status = %#v, %v", status, err)
	}
	_ = session.Close()
	_ = eng.Close()

	reopened, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader, _ := reopened.NewSession()
	result, err := reader.Query(ctx, "SELECT title FROM issues WHERE id = 1")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "durable working row" {
		t.Fatalf("recovered rows = %#v, %v", result.Rows, err)
	}

	if _, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceNativeGit}); !errors.Is(err, repository.ErrWorkingStateDirty) {
		t.Fatalf("native engine with dirty journal = %v", err)
	}
	committed, err := repo.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := committed.Manifest.Tables["issues"]; exists {
		t.Fatal("committed Git snapshot contains uncheckpointed table")
	}
}

func TestJournalCheckpointPublishesOneGitSnapshot(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := repo.Head(ctx)
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	if err := session.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO issues VALUES (1, 'checkpoint me')"); err != nil {
		t.Fatal(err)
	}
	result, err := eng.Checkpoint(ctx, "Triage issues")
	if err != nil || result.Outcome != repository.OutcomeCommitted || result.Commit == before {
		t.Fatalf("checkpoint = %#v, %v", result, err)
	}
	status, err := eng.WorkingState().Status(ctx)
	if err != nil || status.Dirty || status.HeadCommit != result.Commit {
		t.Fatalf("checkpoint status = %#v, %v", status, err)
	}
	native, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	nativeSession, _ := native.NewSession()
	rows, err := nativeSession.Query(ctx, "SELECT title FROM issues")
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0][0] != "checkpoint me" {
		t.Fatalf("checkpoint rows = %#v, %v", rows.Rows, err)
	}
	if again, err := eng.Checkpoint(ctx, "no changes"); err != nil || again.Commit != result.Commit {
		t.Fatalf("unchanged checkpoint = %#v, %v", again, err)
	}
}

func TestJournalRejectsStaleWriterAndIgnoresIncompleteTail(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	a, _ := eng.NewSession()
	if err := a.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	ta, _ := a.Begin(ctx)
	b, _ := eng.NewSession()
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
		t.Fatalf("stale journal commit = %v", err)
	}
	working, _ := repository.OpenWorkingState(repo)
	file, err := os.OpenFile(working.JournalPath(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("RDB")); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := working.Current(ctx); err != nil {
		t.Fatalf("incomplete tail prevented recovery: %v", err)
	}
	writer, _ := eng.NewSession()
	if err := writer.Exec(ctx, "INSERT INTO issues VALUES (3, 'after-tail')"); err != nil {
		t.Fatalf("append after incomplete tail: %v", err)
	}
	verified, err := repository.OpenWorkingState(repo)
	if err != nil { t.Fatal(err) }
	if _, err := verified.Current(ctx); err != nil {
		t.Fatalf("journal after tail repair: %v", err)
	}
}

func TestJournalPostFlushErrorRecoversCommittedTransaction(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	_, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	working := eng.WorkingState()
	working.SetFaultInjector(func(point repository.WorkingFaultPoint) error {
		if point == repository.AfterJournalFlush {
			return errors.New("injected post-flush failure")
		}
		return nil
	})
	session, _ := eng.NewSession()
	err = session.Exec(ctx, "CREATE TABLE durable (id BIGINT PRIMARY KEY)")
	if err == nil {
		t.Fatal("post-flush fault returned success")
	}
	var commitErr *repository.WorkingCommitError
	if !errors.As(err, &commitErr) || commitErr.Outcome != repository.OutcomeCommitted {
		t.Fatalf("post-flush outcome = %v", err)
	}
	recovered, err := working.RecoverTransaction(ctx, commitErr.TransactionID)
	if err != nil || recovered.Outcome != repository.OutcomeCommitted {
		t.Fatalf("recovered working outcome = %#v, %v", recovered, err)
	}
	_ = eng.Close()
	working.SetFaultInjector(nil)
	reopened, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader, _ := reopened.NewSession()
	if _, err := reader.Query(ctx, "SELECT * FROM durable"); err != nil {
		t.Fatalf("durable transaction was lost after post-flush error: %v", err)
	}
}

func TestJournalRecoversCheckpointPublishedBeforeBookkeeping(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	_, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	if err := session.Exec(ctx, "CREATE TABLE durable (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	eng.WorkingState().SetFaultInjector(func(point repository.WorkingFaultPoint) error {
		if point == repository.AfterCheckpointRef {
			return errors.New("injected bookkeeping gap")
		}
		return nil
	})
	result, err := eng.WorkingState().Checkpoint(ctx, "published checkpoint")
	if err == nil || result.Outcome != repository.OutcomeCommitted {
		t.Fatalf("checkpoint = %#v, %v", result, err)
	}
	_ = eng.Close()
	reopened, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err := reopened.WorkingState().Status(ctx)
	if err != nil || status.Dirty || status.HeadCommit != result.Commit {
		t.Fatalf("recovered checkpoint status = %#v, %v", status, err)
	}
}

func TestJournalRejectsChecksummedHistoryCorruption(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	if err := session.Exec(ctx, "CREATE TABLE durable (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	working := eng.WorkingState()
	_ = eng.Close()
	data, err := os.ReadFile(working.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(working.JournalPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	working, _ = repository.OpenWorkingState(repo)
	if _, err := working.Current(ctx); !errors.Is(err, repository.ErrWorkingCorrupt) {
		t.Fatalf("corrupt journal error = %v", err)
	}
}

func TestSeparateJournalEnginesObserveProgressAndRejectStaleWriter(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	a, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sa, _ := a.NewSession()
	if err := sa.Exec(ctx, "CREATE TABLE shared_rows (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	b, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sb, _ := b.NewSession()
	stale, err := sb.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Exec(ctx, "INSERT INTO shared_rows VALUES (2, 'stale')"); err != nil {
		t.Fatal(err)
	}
	if err := sa.Exec(ctx, "INSERT INTO shared_rows VALUES (1, 'winner')"); err != nil {
		t.Fatal(err)
	}
	if err := stale.Commit(ctx); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("cross-engine stale commit = %v", err)
	}
	reader, _ := b.NewSession()
	result, err := reader.Query(ctx, "SELECT title FROM shared_rows")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "winner" {
		t.Fatalf("cross-engine rows = %#v, %v", result.Rows, err)
	}
}

func TestJournalCacheUsesVerifiedOffsetAndDetectsReplacement(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	a, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sa, _ := a.NewSession()
	if err := sa.Exec(ctx, "CREATE TABLE cached_rows (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	working := a.WorkingState()
	working.ResetMetrics()
	if _, err := working.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := working.Status(ctx); err != nil {
		t.Fatal(err)
	}
	metrics := working.Metrics()
	if metrics.CacheHits < 2 || metrics.LoadBytes != 0 || metrics.FullReplays != 0 {
		t.Fatalf("cached metrics = %#v", metrics)
	}

	b, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := b.NewSession()
	if err := sb.Exec(ctx, "INSERT INTO cached_rows VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	working.ResetMetrics()
	status, err := working.Status(ctx)
	if err != nil || status.Generation != 2 {
		t.Fatalf("incremental status = %#v, %v", status, err)
	}
	metrics = working.Metrics()
	if metrics.IncrementalReplays != 1 || metrics.LoadFrames != 2 || metrics.LoadBytes == 0 || metrics.FullReplays != 0 {
		t.Fatalf("incremental metrics = %#v", metrics)
	}

	data, err := os.ReadFile(working.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	replacement := working.JournalPath() + ".replacement"
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, working.JournalPath()); err != nil {
		t.Fatal(err)
	}
	working.ResetMetrics()
	if _, err := working.Status(ctx); err != nil {
		t.Fatal(err)
	}
	metrics = working.Metrics()
	if metrics.FullReplays != 1 || metrics.LoadFrames != 4 {
		t.Fatalf("replacement metrics = %#v", metrics)
	}
}
