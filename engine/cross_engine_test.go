package engine_test

import (
	"context"
	"fmt"
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

// A journal commit by another engine keeps the base commit and tree roots, so
// the reader must not re-validate (decode and walk) unchanged tables; a DDL
// re-validates only the table it changed.
func TestOtherEngineCommitSkipsValidationOfUnchangedTables(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	open := func() *engine.Engine {
		eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}
	writer := open()
	defer writer.Close()
	execAll(t, writer,
		"CREATE TABLE a (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE b (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"INSERT INTO a VALUES (1, 'a'), (2, 'b')",
		"INSERT INTO b VALUES (1, 'x'), (2, 'y')",
	)
	if _, err := writer.Checkpoint(ctx, "persisted trees"); err != nil {
		t.Fatal(err)
	}
	reader := open()
	defer reader.Close()
	if got := queryIDs(t, reader, "SELECT id FROM b ORDER BY id"); got != "[[1] [2]]" {
		t.Fatalf("initial read = %s", got)
	}

	engine.ResetPerformanceCounters()
	for i := 3; i < 8; i++ {
		execAll(t, writer, fmt.Sprintf("INSERT INTO a VALUES (%d, 'w')", i))
		if got := queryIDs(t, reader, "SELECT COUNT(*) FROM a"); got != fmt.Sprintf("[[%d]]", i) {
			t.Fatalf("read after write %d = %s", i, got)
		}
	}
	if got := engine.ReadPerformanceCounters().TablesValidated; got != 0 {
		t.Fatalf("tables validated after other engine's journal commits = %d, want 0", got)
	}

	execAll(t, writer, "CREATE INDEX a_v ON a (v)")
	engine.ResetPerformanceCounters()
	if got := queryIDs(t, reader, "SELECT id FROM b ORDER BY id"); got != "[[1] [2]]" {
		t.Fatalf("read after DDL = %s", got)
	}
	if got := engine.ReadPerformanceCounters().TablesValidated; got != 1 {
		t.Fatalf("tables validated after DDL on one table = %d, want 1", got)
	}
}
