package engine_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
	"github.com/nicbet/repodb/engine"
)

// gitStarts counts the Git processes in a trace2 event log whose argv contains
// command.
func gitStarts(t *testing.T, trace, command string) int {
	t.Helper()
	file, err := os.Open(trace)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Event == "start" && len(event.Argv) > 1 && strings.Contains(strings.Join(event.Argv[1:], " "), command) {
			count++
		}
	}
	return count
}

func treeObjectPath(hash storage.Hash) string {
	return "objects/sha256/" + string(hash[:2]) + "/" + string(hash[2:])
}

// fakeCommit writes (but doesn't publish) a commit on parent whose tree is
// entries plus a manifest.
func fakeCommit(t *testing.T, repo *repository.Repository, parent string, manifest repository.Manifest, entries map[string]string) string {
	t.Helper()
	ctx := context.Background()
	cli := repodbgit.CLI{}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestOID, err := cli.HashObject(ctx, repo.Root, append(data, '\n'))
	if err != nil {
		t.Fatal(err)
	}
	tree := []repodbgit.TreeEntry{{Path: "manifest.json", ObjectID: manifestOID}}
	for path, oid := range entries {
		tree = append(tree, repodbgit.TreeEntry{Path: path, ObjectID: oid})
	}
	treeOID, err := cli.WriteTree(ctx, repo.Root, repo.CommonDir, tree)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := cli.CommitTree(ctx, repo.Root, treeOID, parent, "fake fetched snapshot")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// A checkpoint reuses the base commit's snapshot held by the journal view
// instead of reloading it from Git, and re-validates only the tables whose
// roots changed.
func TestCheckpointReloadsNothingAndValidatesChangedTables(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng,
		"CREATE TABLE a (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE b (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"INSERT INTO a VALUES (1, 'a')",
		"INSERT INTO b VALUES (1, 'b')",
	)
	if _, err := eng.Checkpoint(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	for round := 2; round <= 3; round++ {
		execAll(t, eng, fmt.Sprintf("INSERT INTO a VALUES (%d, 'w')", round))
		// Only the journal's views still reference the base commit.
		runtime.GC()
		trace := filepath.Join(t.TempDir(), "trace2.json")
		t.Setenv("GIT_TRACE2_EVENT", trace)
		engine.ResetPerformanceCounters()
		if _, err := eng.Checkpoint(ctx, "round"); err != nil {
			t.Fatal(err)
		}
		// One ls-tree verifies the published tree; none reloads the base.
		if got := gitStarts(t, trace, "ls-tree"); got != 1 {
			t.Fatalf("round %d: checkpoint ran ls-tree %d times, want 1", round, got)
		}
		if got := engine.ReadPerformanceCounters().TablesValidated; got != 1 {
			t.Fatalf("round %d: checkpoint validated %d tables, want 1 (only a changed)", round, got)
		}
	}
	if got := queryIDs(t, eng, "SELECT COUNT(*) FROM a"); got != "[[3]]" {
		t.Fatalf("rows in a = %s", got)
	}
}

// A fetched snapshot with the same table roots as a validated one, but with
// one of the table's objects stored under a different Git object, is
// validated again and rejected.
func TestValidationCacheDoesNotVouchForSwappedObject(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng,
		"CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"INSERT INTO t VALUES (1, 'genuine'), (2, 'rows')",
	)
	result, err := eng.Checkpoint(ctx, "validated")
	if err != nil {
		t.Fatal(err)
	}
	repo := eng.Repository()
	genuine, err := repo.SnapshotCommit(ctx, result.Commit)
	if err != nil {
		t.Fatal(err)
	}
	engine.ResetPerformanceCounters()
	if err := engine.ValidateSnapshot(ctx, genuine); err != nil {
		t.Fatal(err)
	}
	if got := engine.ReadPerformanceCounters().TablesValidated; got != 0 {
		t.Fatalf("re-validating the checkpointed snapshot validated %d tables, want 0", got)
	}

	cli := repodbgit.CLI{}
	entries, err := cli.ListTreeObjects(ctx, repo.Root, result.Commit)
	if err != nil {
		t.Fatal(err)
	}
	delete(entries, "manifest.json")
	evil, err := cli.HashObject(ctx, repo.Root, []byte("not the data root"))
	if err != nil {
		t.Fatal(err)
	}
	entries[treeObjectPath(genuine.Manifest.Tables["t"].DataRoot)] = evil
	swapped, err := repo.SnapshotCommit(ctx, fakeCommit(t, repo, result.Commit, genuine.Manifest, entries))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ValidateSnapshot(ctx, swapped); !errors.Is(err, repository.ErrCorrupt) {
		t.Fatalf("validate snapshot with swapped data root err = %v, want ErrCorrupt", err)
	}
}

// A schema held only in the local journal doesn't vouch for a fetched
// snapshot that lists the same schema root under a Git object with other
// bytes.
func TestValidationCacheDoesNotVouchWithJournalOnlyObject(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20))")
	working, err := eng.WorkingState().Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ValidateSnapshot(ctx, working); err != nil {
		t.Fatal(err)
	}
	schema := working.Manifest.Tables["t"].SchemaRoot
	if !schema.Valid() {
		t.Fatalf("journal schema root = %q", schema)
	}

	repo := eng.Repository()
	evil, err := repodbgit.CLI{}.HashObject(ctx, repo.Root, []byte("not the schema"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := repository.Manifest{
		FormatVersion: repository.FormatVersion, DefaultDatabase: working.Manifest.DefaultDatabase,
		Tables:  map[string]repository.Table{"t": {SchemaRoot: schema}},
		Objects: []storage.Hash{schema},
	}
	fetched, err := repo.SnapshotCommit(ctx, fakeCommit(t, repo, working.Commit, manifest, map[string]string{treeObjectPath(schema): evil}))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ValidateSnapshot(ctx, fetched); !errors.Is(err, repository.ErrCorrupt) {
		t.Fatalf("validate fetched snapshot err = %v, want ErrCorrupt", err)
	}
}
