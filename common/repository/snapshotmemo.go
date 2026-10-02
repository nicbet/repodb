package repository

import (
	"sync"
	"weak"

	"github.com/nicbet/repodb/common/storage"
)

// snapshotCore is the checked inventory of one data commit and the cache of
// its objects read so far. Every Snapshot of the commit references it, which
// keeps it alive.
type snapshotCore struct {
	manifest   Manifest
	objectSet  map[storage.Hash]struct{}
	objectOIDs map[storage.Hash]string
	cache      *snapshotObjectCache
}

type snapshotMemoKey struct{ repository, commit string }

// snapshotMemo maps loaded commits to their cores through weak pointers. It
// never keeps a snapshot alive by itself: a commit is reused only while some
// caller still holds a snapshot of it. A commit ID names immutable content, so
// a hit skips the manifest and tree checks and reuses every object already
// read and checked.
var snapshotMemo = struct {
	sync.Mutex
	entries map[snapshotMemoKey]weak.Pointer[snapshotCore]
}{entries: make(map[snapshotMemoKey]weak.Pointer[snapshotCore])}

// memoizedSnapshot returns a fresh Snapshot of commit if its core is still
// alive.
func (r *Repository) memoizedSnapshot(commit string) *Snapshot {
	key := snapshotMemoKey{r.Identity(), commit}
	snapshotMemo.Lock()
	pointer, ok := snapshotMemo.entries[key]
	snapshotMemo.Unlock()
	if !ok {
		return nil
	}
	core := pointer.Value()
	if core == nil {
		return nil
	}
	return r.snapshotFromCore(commit, core)
}

// rememberSnapshot records a checked snapshot and sweeps entries whose cores
// were collected.
func (r *Repository) rememberSnapshot(snapshot *Snapshot) {
	if snapshot.core == nil {
		snapshot.core = &snapshotCore{manifest: snapshot.Manifest, objectSet: snapshot.objectSet, objectOIDs: snapshot.objectOIDs, cache: snapshot.cache}
	}
	key := snapshotMemoKey{r.Identity(), snapshot.Commit}
	snapshotMemo.Lock()
	defer snapshotMemo.Unlock()
	for other, pointer := range snapshotMemo.entries {
		if pointer.Value() == nil {
			delete(snapshotMemo.entries, other)
		}
	}
	snapshotMemo.entries[key] = weak.Make(snapshot.core)
}

// snapshotFromCore builds a Snapshot with its own mutable fields; callers set
// generation and pending edits on the snapshots they receive.
func (r *Repository) snapshotFromCore(commit string, core *snapshotCore) *Snapshot {
	return &Snapshot{repo: r, Commit: commit, Manifest: core.manifest, objectSet: core.objectSet, objectOIDs: core.objectOIDs, cache: core.cache, core: core}
}

// resetSnapshotMemo forgets every memoized snapshot, for tests.
func resetSnapshotMemo() {
	snapshotMemo.Lock()
	defer snapshotMemo.Unlock()
	clear(snapshotMemo.entries)
}
