package repository

import (
	"context"
	"os/exec"
	"runtime"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
)

func memoTestRepo(t *testing.T) (*Repository, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", "-b", "main", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Cleanup(func() { repodbgit.CloseReaders(root) })
	repo, err := Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return repo, head
}

// A held snapshot is reused by any Repository of the same Git repository
// without starting a Git process, and every caller gets its own Snapshot.
func TestSnapshotMemoReusesHeldSnapshot(t *testing.T) {
	ctx := context.Background()
	repo, head := memoTestRepo(t)
	resetSnapshotMemo()
	first, err := repo.SnapshotCommit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Discover(ctx, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	before := repodbgit.ProcessCount()
	second, err := other.SnapshotCommit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if started := repodbgit.ProcessCount() - before; started != 0 {
		t.Fatalf("reloading a held snapshot started %d Git processes, want 0", started)
	}
	if first == second {
		t.Fatal("memo handed out the same *Snapshot twice")
	}
	if second.repo != other {
		t.Fatal("memoized snapshot belongs to the wrong Repository")
	}
	second.generation = 7
	if first.Generation() != 0 {
		t.Fatalf("mutating one snapshot changed another: generation = %d", first.Generation())
	}
	third, err := repo.SnapshotCommit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if third.Generation() != 0 || third.PendingEdits() != nil {
		t.Fatalf("memoized snapshot carried working state: generation %d, edits %v", third.Generation(), third.PendingEdits())
	}
	runtime.KeepAlive(first)
}

// The memo holds snapshots weakly: once no caller holds one, it is collected
// and the next load reads from Git again.
func TestSnapshotMemoDoesNotRetainSnapshots(t *testing.T) {
	ctx := context.Background()
	repo, head := memoTestRepo(t)
	resetSnapshotMemo()
	snapshot, err := repo.SnapshotCommit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if repo.memoizedSnapshot(head) == nil {
		t.Fatal("loaded snapshot was not memoized")
	}
	runtime.KeepAlive(snapshot)
	snapshot = nil
	runtime.GC()
	runtime.GC()
	if repo.memoizedSnapshot(head) != nil {
		t.Fatal("memo kept a snapshot alive after every holder dropped it")
	}
	before := repodbgit.ProcessCount()
	if _, err := repo.SnapshotCommit(ctx, head); err != nil {
		t.Fatal(err)
	}
	if repodbgit.ProcessCount() == before {
		t.Fatal("load after collection started no Git process; expected a fresh read")
	}
}

// A commit's own result is memoized, so loading the commit just published
// reads nothing from Git while the result is held.
func TestSnapshotMemoRemembersCommittedSnapshot(t *testing.T) {
	ctx := context.Background()
	repo, _ := memoTestRepo(t)
	base, err := repo.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := repo.BeginSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	result, err := writer.CommitWithOutcomeMessage(ctx, Manifest{DefaultDatabase: "other", Tables: map[string]Table{}}, "rename")
	if err != nil {
		t.Fatal(err)
	}
	before := repodbgit.ProcessCount()
	loaded, err := repo.SnapshotCommit(ctx, result.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if started := repodbgit.ProcessCount() - before; started != 0 {
		t.Fatalf("loading the just-committed snapshot started %d Git processes, want 0", started)
	}
	if loaded.Manifest.DefaultDatabase != "other" {
		t.Fatalf("memoized manifest = %+v", loaded.Manifest)
	}
	runtime.KeepAlive(result.Snapshot)
}
