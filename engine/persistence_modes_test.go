package engine_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

// A repository used in native-git mode opens in journal mode without
// migration, and switching back to native-git needs a checkpoint first.
func TestSwitchingPersistenceModes(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	nativeGit := engine.Options{Persistence: engine.PersistenceNativeGit}
	journalMode := engine.Options{Persistence: engine.PersistenceJournal}
	exec := func(options engine.Options, statements ...string) {
		t.Helper()
		eng, err := engine.OpenWithOptions(ctx, root, options)
		if err != nil {
			t.Fatal(err)
		}
		defer eng.Close()
		s, _ := eng.NewSession()
		defer s.Close()
		for _, statement := range statements {
			if err := s.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	exec(nativeGit, "CREATE TABLE t (id BIGINT PRIMARY KEY)", "INSERT INTO t VALUES (1)")
	exec(journalMode, "INSERT INTO t VALUES (2)")
	if _, err := engine.OpenWithOptions(ctx, root, nativeGit); !errors.Is(err, repository.ErrWorkingStateDirty) {
		t.Fatalf("native-git open over a dirty journal: %v, want ErrWorkingStateDirty", err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, journalMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Checkpoint(ctx, "switch back"); err != nil {
		t.Fatal(err)
	}
	eng.Close()
	exec(nativeGit, "INSERT INTO t VALUES (3)")
	_, s := openJournalEngine(t, ctx, root)
	if got := countRows(t, ctx, s, "t"); got != 3 {
		t.Fatalf("rows after switching both ways = %d, want 3", got)
	}
}

// Backing up a stopped repository by copying it, journal included, keeps
// uncheckpointed rows.
func TestCopiedRepositoryKeepsUncheckpointedRows(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	eng, s := openJournalEngine(t, ctx, root)
	for _, statement := range []string{"CREATE TABLE t (id BIGINT PRIMARY KEY)", "INSERT INTO t VALUES (1)"} {
		if err := s.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := eng.Checkpoint(ctx, "before backup"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close() // RepoDB is stopped

	backup := filepath.Join(t.TempDir(), "backup")
	if err := copyTree(root, backup); err != nil {
		t.Fatal(err)
	}
	restored, rs := openJournalEngine(t, ctx, backup)
	if got := countRows(t, ctx, rs, "t"); got != 2 {
		t.Fatalf("rows in the restored copy = %d, want 2", got)
	}
	if status, err := restored.WorkingState().Status(ctx); err != nil || !status.Dirty {
		t.Fatalf("restored journal status = %#v, %v; want dirty", status, err)
	}
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
