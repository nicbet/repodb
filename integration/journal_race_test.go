package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
)

// twoClones returns two enabled clones sharing a remote, both holding table
// items with row 1.
func twoClones(t *testing.T) (a, b string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	a = filepath.Join(root, "a")
	b = filepath.Join(root, "b")
	git(t, root, "init", "--bare", "--quiet", remote)
	for _, clone := range []string{a, b} {
		git(t, root, "init", "--quiet", "-b", "main", clone)
		git(t, clone, "remote", "add", "origin", remote)
	}
	if _, err := integration.Enable(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	execNative(t, a, "CREATE TABLE items (id BIGINT PRIMARY KEY, label TEXT NOT NULL)", "INSERT INTO items VALUES (1, 'seed')")
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Enable(ctx, b, "origin"); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func execNative(t *testing.T, root string, statements ...string) {
	t.Helper()
	ctx := context.Background()
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	session, _ := eng.NewSession()
	defer session.Close()
	for _, statement := range statements {
		if err := session.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func itemLabels(t *testing.T, eng *engine.Engine) string {
	t.Helper()
	session, _ := eng.NewSession()
	defer session.Close()
	result, err := session.Query(context.Background(), "SELECT id, label FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(result.Rows)
}

// A journal commit that lands between sync's clean check and its head update
// makes sync fail instead of stranding the journal on a replaced head.
func TestSyncRejectsJournalCommitRacingPublication(t *testing.T) {
	for _, path := range []string{"fast-forward", "merge"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			a, b := twoClones(t)
			execNative(t, a, "INSERT INTO items VALUES (2, 'remote')")
			if _, err := integration.Sync(ctx, a, "origin"); err != nil {
				t.Fatal(err)
			}
			if path == "merge" {
				execNative(t, b, "INSERT INTO items VALUES (3, 'local')")
			}
			headBefore := git(t, b, "rev-parse", repository.DataRef)

			journal, err := engine.OpenWithOptions(ctx, b, engine.Options{Persistence: engine.PersistenceJournal})
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			session, _ := journal.NewSession()
			defer session.Close()
			restore := integration.SetBeforePublish(func() {
				if err := session.Exec(ctx, "INSERT INTO items VALUES (4, 'racing')"); err != nil {
					t.Errorf("racing journal commit: %v", err)
				}
			})
			_, syncErr := integration.Sync(ctx, b, "origin")
			restore()
			if !errors.Is(syncErr, integration.ErrWorkingDirty) {
				t.Fatalf("sync error = %v, want ErrWorkingDirty", syncErr)
			}
			if head := git(t, b, "rev-parse", repository.DataRef); head != headBefore {
				t.Fatalf("sync moved the head under a dirty journal: %s -> %s", headBefore, head)
			}
			// A fresh process can still load the journal.
			fresh, err := repository.Open(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			working, _ := repository.OpenWorkingState(fresh)
			if status, err := working.Status(ctx); err != nil || !status.Dirty {
				t.Fatalf("fresh working status = %#v, %v; want dirty", status, err)
			}

			if _, err := journal.Checkpoint(ctx, "racing row"); err != nil {
				t.Fatal(err)
			}
			if _, err := integration.Sync(ctx, b, "origin"); err != nil {
				t.Fatalf("sync after checkpoint: %v", err)
			}
			want := "[[1 seed] [2 remote] [4 racing]]"
			if path == "merge" {
				want = "[[1 seed] [2 remote] [3 local] [4 racing]]"
			}
			if got := itemLabels(t, journal); got != want {
				t.Fatalf("rows after sync = %s, want %s", got, want)
			}
		})
	}
}

// A repository whose head moved under a dirty journal (here by an external
// update-ref, as sync could before rdb-e0c717) recovers with the documented
// procedure: reset the head to the journal's base, checkpoint, sync.
func TestStrandedJournalRecoveryProcedure(t *testing.T) {
	ctx := context.Background()
	a, b := twoClones(t)
	execNative(t, a, "INSERT INTO items VALUES (2, 'remote')")
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	base := git(t, b, "rev-parse", repository.DataRef)
	journal, err := engine.OpenWithOptions(ctx, b, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := journal.NewSession()
	if err := session.Exec(ctx, "INSERT INTO items VALUES (4, 'stranded')"); err != nil {
		t.Fatal(err)
	}
	session.Close()
	journal.Close()

	// Wedge: move the head to the remote's commit under the dirty journal.
	tracking, err := integration.TrackingRef("origin")
	if err != nil {
		t.Fatal(err)
	}
	git(t, b, "fetch", "--quiet", "origin")
	remoteHead := git(t, b, "rev-parse", tracking)
	git(t, b, "update-ref", repository.DataRef, remoteHead)
	_, err = engine.OpenWithOptions(ctx, b, engine.Options{Persistence: engine.PersistenceJournal})
	if !errors.Is(err, repository.ErrWorkingBaseChanged) {
		t.Fatalf("open wedged journal: err = %v, want ErrWorkingBaseChanged", err)
	}
	for _, want := range []string{base, remoteHead, "Recovering a stranded journal"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// Recovery.
	git(t, b, "update-ref", repository.DataRef, base)
	recovered, err := engine.OpenWithOptions(ctx, b, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatalf("open after reset: %v", err)
	}
	defer recovered.Close()
	if _, err := recovered.Checkpoint(ctx, "stranded rows"); err != nil {
		t.Fatal(err)
	}
	if status, err := integration.Sync(ctx, b, "origin"); err != nil || status.Action != "merged" {
		t.Fatalf("sync after recovery = %#v, %v", status, err)
	}
	if got := itemLabels(t, recovered); got != "[[1 seed] [2 remote] [4 stranded]]" {
		t.Fatalf("rows after recovery = %s", got)
	}
}
