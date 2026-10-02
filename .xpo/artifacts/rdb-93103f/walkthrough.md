# Walkthrough: lazy snapshot reads, and checkpoints that stop re-reading the database

## The problem, re-measured

The issue was filed before rdb-2c93ca. By the time this work started, that change had already halved sync at 100k issues (5.7 → 2.7 s per round). So the first step was to measure again, in a Linux container, with a CPU profile and a timing shim around `git`:
- **The checkpoint read the whole database.** `checkpointTypedEdits` calls `SnapshotCommit(base)`, and that load read and SHA-256-checked every object.
- **The memo should have caught that reload, but missed.** The journal holds *derived* snapshots (`applyTypedEditsToSnapshot`, `workingSnapshot`). They copied the base's maps and cache but not its `core`. With nothing holding the core, GC collected it, and the weak memo (rdb-2c93ca) had nothing to return.
- **Validation re-ran on every commit.** `ValidateSnapshot` included the commit in its cache key (a deliberate rdb-187370 choice), so every new commit decoded every table again.

Listing the tree (`ls-tree -r`, 8 ms at 100k) was never the problem. Reading every object was.

## The pieces

### 1. Derived journal views keep the core (`common/repository/working.go`)
This is a one-field change in two constructors: `core: base.core`. A derived view has the same `Commit` and already shares the core's cache, so keeping the core alive costs nothing. Now, while the journal holds a view, the checkpoint's `SnapshotCommit(base)` is a memo hit. `TestCheckpointReloadsNothingAndValidatesChangedTables` asserts it from a trace2 log: a checkpoint runs exactly one `ls-tree`, the post-publication `verifySnapshotTree`. Without the fix there are two.

### 2. Lazy, verified object reads (`common/repository/repository.go`)
`loadSnapshot` now does only the structural checks:
- the manifest decodes strictly and the format version matches;
- the inventory is sorted and unique, and every table root is in it;
- the `ls-tree` listing matches the inventory exactly.

That last check also builds the `hash → Git OID` map. Opening a snapshot reads no object bytes and hashes nothing.

All reads go through `Snapshot.readObjects(ctx, hashes)`:
- **One batch.** It collects the hashes that are not cached, are listed, and have an OID, and reads them in one batch through the long-lived `cat-file` (`git.CLI.ReadObjects`). That replaces the old per-object `ReadTreeFile(commit:path)`.
- **Check before cache.** It hashes each blob and caches only the ones that match their name. It returns the first integrity failure as `ErrCorrupt`. A missing blob (`git.ErrObjectMissing`, now exported) also becomes `ErrCorrupt`. Any other error, such as a cancelled context, passes through unchanged.
- **Never serves unchecked bytes.** `snapshotStore.Get` serves from the cache, or reads that one object. The cache only ever holds checked bytes, so `Get` never returns bytes that weren't checked.

**Batching through a hint.** `storage.Prefetcher` (`Prefetch(ctx, []Hash)`, best-effort) is implemented by:
- `snapshotStore`;
- `Writer`, which forwards the hashes it doesn't hold itself to its base.

It is called from:
- `prolly.walk` (`Reachable`), for each internal node's children;
- full-tree `Iterator()` (the row-validation scan);
- `ImportObjects`.

A cold full validation then costs one cat-file round trip per internal node instead of one per object. Seeking and reverse iterators don't prefetch, because they would read siblings a range query may never visit.

**Memory.** The cache now grows with what's read rather than with the database. During the 100k mutable growth phase, heap went from 422–982 MiB to 249–280 MiB.

### 3. Validation keyed by content, pinned to Git objects (`engine/engine.go`)
**Why the key had to change.** With lazy reads, a commit-keyed validation cache would have made every checkpoint re-read every untouched object from Git, one batch per node. That would undo the gain. So the key is now just the repository plus the table's schema, data and index roots, at version 5.

**The risk, and how it's closed.** rdb-187370 kept the commit in the key so that an object present only in the local journal couldn't vouch for a fetched snapshot. The replacement is `Snapshot.Provides(refs)`:
- each cache entry stores `ObjectRef{Hash, OID}`, the Git object every validated object was read from (empty for journal-only objects);
- a hit counts for a snapshot only if, for each object, it is listed, and either its checked bytes are already in *this* snapshot's cache or this snapshot stores it under the *same* OID.

That closes two holes:
- **A journal-only object** lives only in the local base's cache. A fetched commit gets its own cache, so the object can't vouch for it.
- **A peer that maps an unchanged hash to a different blob** fails the OID comparison. The table is validated again, which reads that blob and fails the integrity check, so the bad blob is caught before any fast-forward.

The two `...DoesNotVouch...` tests cover these cases. Each fails if OID pinning is removed.

**Why cached bytes count regardless of OID.** The spec first allowed this only for objects without an OID, and I changed it in implementation. The cache holds only bytes that matched their name, from three sources: reads through this snapshot's own OIDs, journal objects, and objects our own `Writer.Commit` wrote. A fetched commit shares a cache only when it is a commit we produced ourselves (a memo hit). So cached bytes prove themselves. This also lets a checkpoint commit reuse a validation done while its schema was still journal-only.

**Two additions made during implementation:**
- **Empty tables now get their schema read.** `validateTable` with no data root returned the schema hash without reading it. That was harmless when the eager load checked every blob, but with lazy reads a corrupt schema of an empty fetched table could have been fast-forwarded unchecked. It now goes through `loadTableMetadata`.
- **The cache is bounded by objects as well as tables.** The limits are 1024 tables and 2^20 recorded objects, and the newest entry is always kept. Every checkpoint of a changed big table adds an entry, and entries now carry OIDs, so a table-count bound alone could have reached gigabytes.

## The trust model (decided with the user)

Integrity is now checked on read, not at open (D1).
- **Callers never see unchecked bytes.** Every byte is still checked before use.
- **Corruption in unread regions surfaces later.** It is reported by the first read or validation that reaches it.
- **Fetched data is checked before it is published.** `ValidateSnapshot` reads everything reachable from the roots of tables whose cached validation doesn't apply. Inventories equal those reachable graphs, because every publication path calls `RetainOnly`.

If you want a full check on demand, that is rdb-a8357d (`repodb check`).

## Results (Linux container, 3 rounds, compared with dd3a4fc)

| | before | after |
|---|---|---|
| mutable sync per round, 100k | 2.63–2.71 s | 2.09–2.12 s |
| growth-phase heap, 100k | 422–982 MiB | 249–280 MiB |
| append sync per round, 100k | 324–502 ms | 330–391 ms |
| 1k groups | — | unchanged or slightly faster |
| peer pull, 100k | 2.30 s | 2.33 s |

- **Peer pull** goes into a fresh clone, which must read and validate everything, so it is inherently O(database).
- **The spec's ≤ 1.8 s target was missed.** What's left of each round is filed:
  - the checkpoint index rebuild (rdb-8779d9, ~0.28 s);
  - validation of changed tables (rdb-a8200b, ~0.1 s);
  - Git writing about 11 MB per round (rdb-7f0684 for the O(objects) metadata share).

## Files
- `common/repository/working.go`: core propagation.
- `common/repository/repository.go`: lazy load, `readObjects`, `Get`/`Prefetch`, `Writer.Prefetch`, `ObjectRef`/`ObjectRefs`/`Provides`.
- `common/repository/snapshotmemo.go`: comments only.
- `common/storage/store.go`: `Prefetcher`.
- `common/prolly/tree.go`: `prefetchChildren` in `walk` and `Iterator()`.
- `common/git`: exported `ErrObjectMissing`.
- `engine/engine.go`: the validation cache and the empty-table schema read.
- Tests: `common/repository/lazyload_internal_test.go`, `engine/lazy_validation_test.go`.
- Docs: `docs/architecture.md`, `docs/testing.md`.
