package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/storage"
)

// commitBlobs publishes blobs as the only objects of a new snapshot on head.
func commitBlobs(t *testing.T, repo *Repository, blobs ...string) (*Snapshot, []storage.Hash) {
	t.Helper()
	ctx := context.Background()
	writer, err := repo.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([]storage.Hash, len(blobs))
	for i, blob := range blobs {
		if hashes[i], err = writer.Put(ctx, []byte(blob)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := writer.Commit(ctx, Manifest{DefaultDatabase: "repodb", Tables: map[string]Table{}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, hashes
}

// coldSnapshot loads commit through a new Repository with an empty memo, as a
// fresh process would.
func coldSnapshot(t *testing.T, root, commit string) *Snapshot {
	t.Helper()
	ctx := context.Background()
	resetSnapshotMemo()
	repo, err := Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.SnapshotCommit(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func cachedCount(s *Snapshot) int {
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()
	return len(s.cache.data)
}

// rewriteObject publishes nothing: it returns a commit whose tree is
// snapshot's tree with hash pointing at the blob oid.
func rewriteObject(t *testing.T, snapshot *Snapshot, hash storage.Hash, oid string) string {
	t.Helper()
	ctx := context.Background()
	repo := snapshot.repo
	cli := repodbgit.CLI{}
	objects, err := cli.ListTreeObjects(ctx, repo.Root, snapshot.Commit)
	if err != nil {
		t.Fatal(err)
	}
	objects[objectPath(hash)] = oid
	entries := make([]repodbgit.TreeEntry, 0, len(objects))
	for path, entryOID := range objects {
		entries = append(entries, repodbgit.TreeEntry{Path: path, ObjectID: entryOID})
	}
	tree, err := cli.WriteTree(ctx, repo.Root, repo.CommonDir, entries)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := cli.CommitTree(ctx, repo.Root, tree, snapshot.Commit, "rewritten test snapshot")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// Loading a snapshot checks its inventory but reads no object; Get reads and
// checks one object, and Prefetch reads the rest in one batch.
func TestLoadSnapshotReadsObjectsOnFirstUse(t *testing.T) {
	ctx := context.Background()
	repo, _ := memoTestRepo(t)
	published, hashes := commitBlobs(t, repo, "one", "two", "three")

	snapshot := coldSnapshot(t, repo.Root, published.Commit)
	if got := cachedCount(snapshot); got != 0 {
		t.Fatalf("objects read by loading a snapshot = %d, want 0", got)
	}
	data, err := snapshot.Store().Get(ctx, hashes[0])
	if err != nil || string(data) != "one" {
		t.Fatalf("Get = %q, %v", data, err)
	}
	if got := cachedCount(snapshot); got != 1 {
		t.Fatalf("objects cached after one Get = %d, want 1", got)
	}
	before := repodbgit.ProcessCount()
	storage.Prefetch(ctx, snapshot.Store(), hashes)
	if got := cachedCount(snapshot); got != len(hashes) {
		t.Fatalf("objects cached after Prefetch = %d, want %d", got, len(hashes))
	}
	for i, hash := range hashes {
		if data, err := snapshot.Store().Get(ctx, hash); err != nil || storage.Sum(data) != hash {
			t.Fatalf("Get(%d) after Prefetch = %q, %v", i, data, err)
		}
	}
	if started := repodbgit.ProcessCount() - before; started != 0 {
		t.Fatalf("Prefetch and cached Gets started %d Git processes, want 0", started)
	}
}

// A tree entry that names an object but holds other bytes loads (only the
// inventory is checked) and fails with ErrCorrupt when the object is read.
func TestSnapshotGetDetectsAlteredObject(t *testing.T) {
	ctx := context.Background()
	repo, _ := memoTestRepo(t)
	published, hashes := commitBlobs(t, repo, "genuine", "untouched")
	evil, err := repodbgit.CLI{}.HashObject(ctx, repo.Root, []byte("altered"))
	if err != nil {
		t.Fatal(err)
	}
	altered := rewriteObject(t, published, hashes[0], evil)

	snapshot := coldSnapshot(t, repo.Root, altered)
	if data, err := snapshot.Store().Get(ctx, hashes[1]); err != nil || string(data) != "untouched" {
		t.Fatalf("Get(untouched) = %q, %v", data, err)
	}
	_, err = snapshot.Store().Get(ctx, hashes[0])
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "failed integrity check") {
		t.Fatalf("Get(altered) err = %v, want ErrCorrupt integrity failure", err)
	}
	if got := cachedCount(snapshot); got != 1 {
		t.Fatalf("objects cached = %d, want only the genuine one", got)
	}
	// A batch with the altered object still caches the genuine ones.
	storage.Prefetch(ctx, snapshot.Store(), hashes)
	if _, err := snapshot.Store().Get(ctx, hashes[0]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get(altered) after Prefetch err = %v, want ErrCorrupt", err)
	}
}

// A listed object whose Git blob is gone fails with ErrCorrupt on read.
func TestSnapshotGetDetectsMissingObject(t *testing.T) {
	ctx := context.Background()
	repo, _ := memoTestRepo(t)
	published, hashes := commitBlobs(t, repo, "doomed")
	oid := published.objectOIDs[hashes[0]]
	repodbgit.CloseReaders(repo.Root)
	if err := os.Remove(filepath.Join(repo.CommonDir, "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}

	snapshot := coldSnapshot(t, repo.Root, published.Commit)
	if _, err := snapshot.Store().Get(ctx, hashes[0]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get(missing) err = %v, want ErrCorrupt", err)
	}
}

// Provides accepts objects a snapshot holds as checked bytes or under the same
// Git object ID, and refuses other Git objects and memory-only objects it
// lacks.
func TestSnapshotProvides(t *testing.T) {
	ctx := context.Background()
	repo, _ := memoTestRepo(t)
	published, hashes := commitBlobs(t, repo, "a", "b")
	refs := published.ObjectRefs(hashes)
	if !published.Provides(refs) {
		t.Fatal("snapshot does not provide its own objects")
	}

	// A memory-only object (OID "") is provided only by a snapshot holding
	// its bytes.
	journal := ObjectRef{Hash: storage.Sum([]byte("journal-only"))}
	cold := coldSnapshot(t, repo.Root, published.Commit)
	if !cold.Provides(refs) {
		t.Fatal("unread snapshot does not provide objects under the same OIDs")
	}
	if cold.Provides([]ObjectRef{journal}) {
		t.Fatal("snapshot provides an object it doesn't list")
	}
	listed := &Snapshot{repo: cold.repo, Commit: cold.Commit, objectSet: map[storage.Hash]struct{}{journal.Hash: {}}, objectOIDs: map[storage.Hash]string{journal.Hash: "1234"}, cache: &snapshotObjectCache{data: map[storage.Hash][]byte{}}}
	if listed.Provides([]ObjectRef{journal}) {
		t.Fatal("unread Git object vouched for by a memory-only validation")
	}
	listed.cache.data[journal.Hash] = []byte("journal-only")
	if !listed.Provides([]ObjectRef{journal}) {
		t.Fatal("checked bytes in the snapshot's cache are not provided")
	}

	// The same hash under another Git object is not provided until read.
	evil, err := repodbgit.CLI{}.HashObject(ctx, repo.Root, []byte("not a"))
	if err != nil {
		t.Fatal(err)
	}
	swapped := coldSnapshot(t, repo.Root, rewriteObject(t, published, hashes[0], evil))
	if swapped.Provides(refs) {
		t.Fatal("snapshot provides an object stored under a different Git object")
	}
}
