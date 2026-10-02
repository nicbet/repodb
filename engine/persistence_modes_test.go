package engine_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

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

// Acknowledged journal commits survive the process being killed, at every
// durability level: even without a flush, the OS still holds the data.
func TestJournalCommitsSurviveProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses SIGKILL")
	}
	for _, level := range []repository.Durability{repository.DurabilityFull, repository.DurabilityNormal, repository.DurabilityOff} {
		t.Run(string(level), func(t *testing.T) {
			ctx := context.Background()
			root := initJournalRepo(t, ctx)
			cmd := exec.Command(os.Args[0], "-test.run", "^TestJournalKillHelper$")
			cmd.Env = append(os.Environ(), "REPODB_KILL_HELPER_ROOT="+root, "REPODB_KILL_HELPER_DURABILITY="+string(level))
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			acknowledged := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if scanner.Text() == "committed" {
						acknowledged <- true
						return
					}
				}
				acknowledged <- false
			}()
			select {
			case ok := <-acknowledged:
				if !ok {
					_ = cmd.Process.Kill()
					t.Fatal("helper exited before committing")
				}
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("helper did not commit in time")
			}
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			_, s := openJournalEngine(t, ctx, root)
			if got := countRows(t, ctx, s, "t"); got != 20 {
				t.Fatalf("rows after kill at %s durability = %d, want 20", level, got)
			}
		})
	}
}

// TestJournalKillHelper runs in a subprocess for
// TestJournalCommitsSurviveProcessKill: it commits 20 rows, reports it, and
// waits to be killed without closing anything.
func TestJournalKillHelper(t *testing.T) {
	root := os.Getenv("REPODB_KILL_HELPER_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	ctx := context.Background()
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal, Durability: repository.Durability(os.Getenv("REPODB_KILL_HELPER_DURABILITY"))})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 20; i++ {
		if err := s.Exec(ctx, "INSERT INTO t VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("committed")
	time.Sleep(time.Minute)
}

func TestEngineDurabilityOption(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	for _, level := range []repository.Durability{"", repository.DurabilityFull, repository.DurabilityOff} {
		eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal, Durability: level})
		if err != nil {
			t.Fatal(err)
		}
		want := level
		if want == "" {
			want = repository.DurabilityNormal
		}
		if got := eng.WorkingState().Durability(); got != want {
			t.Errorf("journal durability = %q, want %q", got, want)
		}
		eng.Close()
	}
	if _, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal, Durability: "fast"}); err == nil {
		t.Error("engine accepted an unknown durability")
	}
}
