# rdb-93103f: Lazy snapshot object reads; checkpoint and sync stop re-reading the database

## What

Opening a snapshot (`Repository.loadSnapshot`) should cost O(tree entries) of cheap metadata work: decode the manifest and list the tree. Object bytes are read and SHA-256-checked when a caller first `Get`s them, not up front. Then remove the two paths that would still touch every object on each checkpoint or sync:

1. the checkpoint's `SnapshotCommit(base)` reload, which misses the weak memo;
2. full SQL re-validation of every table under each new commit.

## Why: measurements on current main (after rdb-2c93ca)

Linux container, journal mode, `mutable` group at 100k issues, 3 growth rounds (600 edits each, then sync to a local bare remote).

| | issue (before rdb-2c93ca) | now |
|---|---|---|
| sync per round, 100k | 5.7 s | **2.7 s** |
| peer pull after growth, 100k | 6.2 s | 2.3 s |
| initial publish, 100k | 16 s | 13.5 s |

One round of sync (2.8 s), measured with a timing shim around `git`:

| phase | time | scales with |
|---|---|---|
| checkpoint before writing objects: `SnapshotCommit(base)` reads and hashes **every** object through cat-file, `prolly.Apply`, index rebuild | ~1.1 s | database (the load and the index rebuild) |
| `hash-object -w` of new objects | ~0.47 s | bytes changed |
| `write-tree` | ~0.12 s | entries (256 subtrees) |
| post-commit `ValidateSnapshot`: all tables, because the commit is in the key | ~0.11 s | database |
| `push` | ~0.9 s | bytes changed |
| `ls-tree -r` (each) | 8 ms | entries (negligible) |

CPU profile (sync label, 3 rounds): `checkpointIndexTrees → rebuildIndexesFromTree` 0.80 s, `loadSnapshot` 0.40 s (`storage.Sum` 0.23 s, `ReadObjects` 0.15 s; cat-file's own time comes on top), `ValidateSnapshot` 0.31 s.

**Why the checkpoint reload misses the memo.** The journal's views are *derived* snapshots (`applyTypedEditsToSnapshot` and `workingSnapshot` in `working.go`). They share the base's cache and OIDs but leave `core` nil. Once the checkpoint's result snapshot is replaced by a derived view, nothing references the core, GC collects it, and `checkpointTypedEdits → SnapshotCommit(snapshot.Commit)` reloads the whole commit.

## How

### 1. Derived snapshots keep their base core alive (`common/repository/working.go`)
`applyTypedEditsToSnapshot` and `workingSnapshot` set `core: base.core`. A derived snapshot has the same `Commit`, so while the journal holds a view, `SnapshotCommit(view.Commit)` is a memo hit. This doesn't change what is retained: the derived snapshot already shares the core's cache.

### 2. Lazy object reads (`common/repository/repository.go`)
`loadSnapshot` keeps every check except the object reads:
- the manifest decode, format version, sorted and unique inventory, and `validateManifestInventory`;
- `ls-tree -r`: every listed object has exactly one tree entry and there are no unlisted entries. This also yields the `hash → OID` map.

It no longer calls `ReadObjects` or hashes anything. The cache starts empty.

`snapshotStore.Get`, on a cache miss:
- reads by **OID** through `git.CLI.ReadObjects` (the long-lived cat-file), not `ReadTreeFile(commit:path)`;
- checks `storage.Sum(data) == hash` and returns an `ErrCorrupt`-wrapped "failed integrity check" error otherwise;
- returns an `ErrCorrupt` error if the blob is missing;
- stores the bytes in the shared cache only after the check passes.

**Batching.** Add an optional `storage.Prefetcher` interface, `Prefetch(ctx, []Hash)`. It is best-effort: failures are left for `Get` to report. `snapshotStore` and `Writer` implement it: one `ReadObjects` batch for uncached OIDs, each verified as above. `prolly.walk` (used by `Reachable`) and full-tree `Iterator()` prefetch all children of an internal node before descending. A cold full validation then makes one cat-file round trip per internal node, not one per object. Point reads, `Apply`, seeking iterators (`IteratorFrom`) and reverse iterators stay unbatched; they touch one path. `ImportObjects` prefetches its source objects.

### 3. Table validation is not keyed by commit; hits are pinned to OIDs (`engine/engine.go`)
The key becomes (repository identity, `snapshotValidationVersion`, schema root, data root, sorted index roots). The version goes to 5.

The cached value records each reachable object together with the **Git OID it was validated under** (`repository.ObjectRef`). Journal-only objects have no OID and record an empty one.

On a hit, `ValidateSnapshot` accepts the table for this snapshot only if the snapshot provides every cached object (`Snapshot.Provides`):
- the object is in `objectSet`; and
- either its bytes are already in this snapshot's cache, or the snapshot's OID for it equals the recorded OID. The cache holds only bytes that hashed to their name: bytes read through this snapshot's OIDs, journal-only objects, and objects our own publication wrote. *(Revised during implementation: the original wording allowed cached bytes only for objects without an OID. Any checked bytes are equally sound, and this also lets a checkpoint commit reuse a validation done while its schema was journal-only.)*

If any object fails, the table is validated again and the entry is replaced with the new OIDs.

**Bound.** The cache keeps at most 1024 tables and 2^20 recorded objects (LRU), and always keeps the newest entry. Every checkpoint of a changed big table adds an entry, now carrying OIDs too, so a count of entries alone could grow to gigabytes.

**Empty tables.** `validateTable` reads and decodes the schema of a table with no data root. Before, the eager load was the only check on that blob. *(Added during implementation.)*

- **Sound for fetched snapshots.** A fetched snapshot has its own core and cache, so a journal-only object can't vouch for it (the rdb-187370 concern). A peer tree that maps an unchanged object hash to a *different* blob fails the OID match and is re-validated by reading that blob, so it is caught before any fast-forward.
- **A memo hit on our own commit is fine.** A pulled commit that we produced ourselves (after a push) returns our core, which shares the journal cache. Every object of a published commit has an OID (`verifySnapshotTree`), so this is still sound.
- **Content validity depends only on the roots** (content-addressed). The OID pinning makes "these are the bytes that were validated" hold per snapshot.

`validatedTableObjects` uses the same check and still returns `[]storage.Hash` to its callers. A miss behaves as now.

Effect: the post-checkpoint `ValidateSnapshot` and pull validation only re-validate tables whose roots changed. Pruning unchanged subtrees inside a changed table is rdb-a8200b.

### 4. Docs
In `docs/architecture.md`, update "Opening a snapshot":
- loading checks the manifest and tree inventory;
- objects are read and verified on first use;
- memory holds only objects read.

Also update:
- "Reading objects";
- "Reusing validated snapshots", renamed to "Reusing loaded snapshots" (derived journal views keep the core alive);
- "SQL validation" (the new key, the availability check, the bound).

Remove the rdb-93103f limitation sentence. `docs/testing.md`: describe the corruption coverage.

## Decisions (confirmed by the user, 2026-10-02)

- **D1: Integrity is checked on read, not at open.** No bytes are ever served unverified. But a corrupt or missing object that is never read is no longer detected when a snapshot is opened. For data that RepoDB *propagates* (fast-forward, merge, push of fetched commits), `ValidateSnapshot` still reads every object reachable from the table roots. Inventories are bounded to exactly those graphs (`RetainOnly` on every publication path), so fetched data stays fully checked once per distinct table content.
- **D2: Reverse rdb-187370's "commit stays in the key"**, replacing it with the OID-pinned availability check in §3. This is what stops each checkpoint and pull from re-validating unchanged tables. With lazy reads, it also avoids reading the whole database one object at a time.
- **D3: The trust root.** The first load in a cold process (CLI runs) no longer reads anything up front. Opening a 100k database in the CLI becomes manifest + `ls-tree` + what the query reads.
- **D4: An on-demand full check is the mitigation for D1.** `repodb check` is filed as rdb-a8357d and is not part of this issue. `docs/architecture.md` names it as the way to verify every object.

## Edge cases
- **Missing blob for a listed entry:** `Get` fails with `ErrCorrupt` and the object ID. No existing test asserted that open detects an altered or missing object; new tests cover detection on read.
- **Cancelled context during `Get`:** the reader kills the process (existing behaviour); nothing is cached.
- **Concurrent `Get` of the same hash:** both may read; the cache write is idempotent.
- **Journal-only objects (schemas):** these are still put into the cache by `working.go`, never read from Git, so they need no OID.
- **`Writer.Commit` verification (`verifySnapshotTree`):** unchanged.
- **`ImportObjects`:** prefetches, then goes through `Store().Get`, so it is lazily verified; it keeps the source OID.

## Out of scope (filed)
- rdb-8779d9: checkpoint rebuilds every secondary index from the full data tree.
- rdb-7f0684: per-commit metadata is O(objects) (manifest inventory, the 256 tree buckets).
- rdb-a8200b: validation re-walks unchanged subtrees of a changed table.
- rdb-a8357d: the `repodb check` command (the mitigation for D1).

## Acceptance criteria
- [x] `loadSnapshot` reads no object: the cache is empty after a cold load.
- [x] A test of reading an object whose blob content was altered fails with `ErrCorrupt` on `Get`. A missing blob also fails on `Get`.
- [x] A journal checkpoint in a test loads no snapshot from Git: one `ls-tree` (the post-publication verification), asserted with trace2.
- [x] Two consecutive checkpoints that change one table re-validate only that table (`TablesValidated`).
- [x] A fetched snapshot is never validated by a cache entry it doesn't have the objects for. Tests:
  - (a) a journal-only schema, and a fetched commit listing the same schema root under a blob with other bytes: rejected with `ErrCorrupt`;
  - (b) a fetched tree that maps an unchanged data root to a different blob: rejected with `ErrCorrupt`.
- [x] dbbench `mutable` and `append` at 100k (Linux container, 3 rounds): the per-round sync drops by the checkpoint load, and the 1k numbers don't regress. *Outcome: 2.7 → 2.1 s, and growth-phase heap 422–982 → 249–280 MiB. The ≤ 1.8 s target was not met; the remainder is rdb-8779d9, rdb-a8200b and Git writes (rdb-7f0684). The user accepted this.*
- [x] `make test` and `make lint` pass.
