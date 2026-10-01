# rdb-fb3d12 walkthrough: content-local Prolly chunk boundaries

## How it was found

`BenchmarkJournalCheckpoint/batch=10` took ~1.1 s while batch=1 and batch=100 took ~0.19 s. A Git trace showed the extra time inside `git hash-object -w` and `git write-tree`. A probe then showed checkpoint time tracking the number of new Git objects, which swung from 12 to 231 with no relation to the edit size: 2 updated keys wrote 132 objects, 100 keys wrote 13. In `common/prolly` alone, `Apply` rewrote 28–130 nodes of a 127-leaf tree for any batch.

## Why it happened

A Prolly tree's chunk boundaries must depend only on local content, so an edit disturbs only nearby chunks. Ours didn't:
- **The leaf boundary hash covered the value** (`fnv(key, 0, value)`), so updating a value could move its chunk's boundary.
- **The hash accumulated since the last cut** (`rolling = rotl(rolling, 1) ^ fingerprint`, reset to 0 at each cut). Once one boundary moved, every later cut depended on the new starting point, and the chunking only re-synced when a new cut happened to land on an old boundary. That takes about one average chunk's worth of chunks. `Apply`'s early exit ("stop when the chunker flushes exactly at an old leaf end") therefore ran on for dozens of leaves.
- **The internal boundary hash covered the child's hash**, so internal boundaries moved whenever a child changed.

The output was still canonical, so all the history-independence tests passed. Only the cost was wrong, which is why this went unnoticed.

## The fix (`common/prolly/tree.go`)

One rule, shared by `chunk()` (Build and internal levels), `leafChunker` (Apply) and `SortedBuilder`, via `shouldCut(size, hash, options)`:
- a chunk ends after an item when it reaches `MaxChunkEntries`, or when it has at least `MinChunkEntries` and the item's boundary hash has its low `BoundaryBits` bits zero;
- the boundary hash is the FNV-64a of the **entry's key** (`entryBoundaryHash`) or of the **link's MaxKey** (`linkBoundaryHash`);
- no state is carried across items. The `rolling` fields are gone.

Consequences:
- An **update** never moves a boundary: `Apply` rewrites exactly one leaf and its path (3 nodes at 10k entries).
- An **insert or delete** changes one chunk's size. The size-based cuts can shift up to the next key-hash boundary, where the chunking re-syncs: median 3 nodes, worst 8 over 271 positions.
- `Apply` itself didn't change. Its early exit now fires right after the edited region.

## Format

Every tree's shape changes, so `repository.FormatVersion` goes 3 → 4. Old repositories are refused with the existing "unsupported RepoDB format" error, and alpha needs no migration (rdb-92cd4a covers future ones). Mixing shapes would break canonicality: two clones with equal content could have different roots, and merge relies on equal roots to skip unchanged tables. The journal format is unchanged, because journal records hold rows, not tree shapes.

## Evidence

| | before | after |
|---|---|---|
| Checkpoint batch=1 / 10 / 100 | ~195 / ~1,200 / ~200 ms | ~200 / 207–298 / 200–329 ms |
| New objects per checkpoint (1–100 keys) | 12–231 | 12–14 |
| Native-git write batches, mutations = 1 / 1000 | ~350 / ~650 ms | ~150 / ~345 ms |
| dbbench native-git 50k, update_batch_10 max | 1.4–2.9 s | 165 ms |

The dbbench native-git "fsync stalls" in rdb-c85855 were this bug, the same two-writers-slow signature, and are gone with it.

## Tests

`TestApplyRewritesOnlyTouchedChunks` bounds the nodes written per edit (updates ≤ 1 per level, inserts and deletes ≤ 3 per level, k updates ≤ k per level) and checks each result against a fresh `Build`. A test that only checks canonical output can't catch a cost regression like this one; this one can. All existing Prolly, repository, engine and integration tests pass at format 4.
