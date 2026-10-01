# rdb-fb3d12: Content-local Prolly chunk boundaries

## What / Why

See the issue. In short:
- **Leaf boundaries** hashed key and value and accumulated since the last cut. A value update could move a boundary, and every later boundary then shifted until one happened to land on an old one. `Apply` rewrote 28–130 nodes for 1–100 updated keys in a 10k-entry tree.
- **Internal boundaries** hashed the child's hash, so they moved whenever a child changed.

Native-git commits and journal checkpoints paid for every extra node with a fsync'd Git object.

## How (`common/prolly/tree.go`)

**Boundary rule.** It is shared by `chunk()` (Build and internal levels), `leafChunker` (Apply) and `SortedBuilder`, through `shouldCut(size, hash, options)`:
- A chunk ends after item `i` when `size ≥ MaxChunkEntries`, or when `size ≥ MinChunkEntries` and `hash & mask == 0`.
- **Leaves:** `entryBoundaryHash` = FNV-64a of the entry's **key** only.
- **Internal levels:** `linkBoundaryHash` = FNV-64a of the link's **MaxKey** only.
- No accumulation: the `rolling` state is gone from `leafChunker`, `SortedBuilder` and `linkBuffer`. `Options` is unchanged (32/128, 6 bits).

**Why this is local.** The cut decision for an item depends only on that item and the current chunk's size.
- **Value updates** never move a boundary: only the edited leaf and its path change.
- **Inserts and deletes** change sizes within their chunk. The size-based cuts can shift up to the next key-hash boundary, and the chunking re-syncs there.

**`Apply`.** No algorithmic change: it starts at the first affected leaf and stops once the chunker flushes exactly at an old leaf end after the last edit.

**Format.** Tree shapes change, so `repository.FormatVersion` goes from 3 to 4. Format-3 repositories are refused with the existing "unsupported RepoDB format" error, and native-git commit subjects read `RepoDB snapshot v4`. Alpha needs no migration (rdb-92cd4a covers future migrations). A version bump keeps trees canonical: two clones with the same content get the same root, which merge relies on to skip unchanged tables. The journal format is unchanged: its records hold rows, not tree shapes.

## Tests

- **Existing Prolly tests** pass unchanged: deterministic builds, streaming builder equals bulk build, and `Apply` equals a fresh `Build` for random edit sequences from 0 to 10k entries.
- **New `TestApplyRewritesOnlyTouchedChunks`:** a counting store over a 10k-entry tree (depth 3).
  - Updates at the first, middle and last key write ≤ 3 nodes (exactly one per level).
  - Inserts and deletes at those positions write ≤ 9 nodes.
  - Updating 2, 10 and 100 adjacent keys writes ≤ k × 3.
  - Every result is checked against a fresh `Build`.
  - The insert/delete bound comes from a measured distribution over 271 positions: updates always 3; inserts median 3, worst 7; deletes median 3, worst 8. Before the fix, a single-key update wrote 47–106 nodes.
- The repository, engine, integration and server suites pass at format 4; no fixture pinned format 3.

## Results

| | before | after |
|---|---|---|
| `BenchmarkJournalCheckpoint` batch=1 / 10 / 100 (1x, 5 runs) | ~195 / **~1,200** / ~200 ms | ~200 / 207–298 / 200–329 ms |
| New loose objects per checkpoint, k = 1..100 updated keys | 12–231 | 12–14 |
| `BenchmarkSQLWriteBatches` native-git, mutations = 1 / 10 / 100 / 1000 (20x, 3 runs) | ~350 / ~430 / ~450 / ~650 ms | ~150 / ~135 / ~155 / ~345 ms |

## Docs

- `docs/architecture.md`: the Prolly boundary rule and why edits stay local; `Apply`'s cost for an update; the commit subject `RepoDB snapshot v4`.
- `docs/testing.md`: the edit-locality test.
- `docs/benchmarks/latest.md` is not touched; the next scorecard refresh will show the effect.
- rdb-c85855: re-checked with a native-git 50k dbbench run (see the issue comment).

## Acceptance

As in the issue. `make test` and `make lint` pass.
