package engine_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

// Each engine opens its own repository.Repository, like separate processes.
func TestReadersObserveOtherEngineWrites(t *testing.T) {
	for _, tc := range []struct {
		name        string
		persistence engine.PersistenceMode
		checkpoint  bool
	}{
		{"journal-dirty", engine.PersistenceJournal, false},
		{"journal-clean", engine.PersistenceJournal, true},
		{"native-git", engine.PersistenceNativeGit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := gitRepository(t)
			if _, err := repository.Init(ctx, root); err != nil {
				t.Fatal(err)
			}
			open := func() *engine.Engine {
				eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: tc.persistence})
				if err != nil {
					t.Fatal(err)
				}
				return eng
			}
			writer := open()
			defer writer.Close()
			execAll(t, writer, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20))", "INSERT INTO t VALUES (1, 'a')")
			if tc.checkpoint {
				if _, err := writer.Checkpoint(ctx, "clean"); err != nil {
					t.Fatal(err)
				}
			}
			reader := open()
			defer reader.Close()
			// The reader writes first: the stale-snapshot shortcut only engaged
			// after an engine's own state change.
			execAll(t, reader, "INSERT INTO t VALUES (10, 'r')")
			if tc.checkpoint {
				if _, err := reader.Checkpoint(ctx, "reader clean"); err != nil {
					t.Fatal(err)
				}
			}
			if got := queryIDs(t, reader, "SELECT id FROM t ORDER BY id"); got != "[[1] [10]]" {
				t.Fatalf("initial read = %s", got)
			}

			execAll(t, writer, "INSERT INTO t VALUES (2, 'b')")
			if tc.checkpoint {
				if _, err := writer.Checkpoint(ctx, "clean again"); err != nil {
					t.Fatal(err)
				}
			}
			if got := queryIDs(t, reader, "SELECT id FROM t ORDER BY id"); got != "[[1] [2] [10]]" {
				t.Fatalf("read after other engine's write = %s, want [[1] [2] [10]]", got)
			}
			// The reader's next write builds on the fresh snapshot instead of
			// failing as a stale writer.
			execAll(t, reader, "INSERT INTO t VALUES (3, 'c')")
			if got := queryIDs(t, writer, "SELECT id FROM t ORDER BY id"); got != "[[1] [2] [3] [10]]" {
				t.Fatalf("writer read after reader's write = %s, want [[1] [2] [3] [10]]", got)
			}
		})
	}
}

// A data-ref change made outside RepoDB must be observed without a restart.
func TestHeadObservesExternalRefUpdate(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.Head(ctx)
	if err != nil || first == "" {
		t.Fatalf("head = %q, %v", first, err)
	}
	eng, err := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, eng, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	second, err := repo.Head(ctx)
	if err != nil || second == first {
		t.Fatalf("head after write = %q (was %q), %v", second, first, err)
	}
	_ = eng.Close()

	// Roll the ref back with plain Git, as another tool or process could.
	if out, err := exec.Command("git", "-C", root, "update-ref", repository.DataRef, first).CombinedOutput(); err != nil {
		t.Fatalf("git update-ref: %v: %s", err, out)
	}
	if got, err := repo.Head(ctx); err != nil || got != first {
		t.Fatalf("head after external update-ref = %q, want %q (%v)", got, first, err)
	}
	// pack-refs moves the ref into packed-refs; the value must still resolve.
	if out, err := exec.Command("git", "-C", root, "pack-refs", "--all").CombinedOutput(); err != nil {
		t.Fatalf("git pack-refs: %v: %s", err, out)
	}
	if got, err := repo.Head(ctx); err != nil || got != first {
		t.Fatalf("head after pack-refs = %q, want %q (%v)", got, first, err)
	}
	if out, err := exec.Command("git", "-C", root, "update-ref", repository.DataRef, second).CombinedOutput(); err != nil {
		t.Fatalf("git update-ref: %v: %s", err, out)
	}
	if got, err := repo.Head(ctx); err != nil || got != second {
		t.Fatalf("head after packed update-ref = %q, want %q (%v)", got, second, err)
	}
}
