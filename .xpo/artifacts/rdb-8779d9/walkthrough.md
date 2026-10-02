# Walkthrough: incremental secondary-index checkpoint

## What was built and why

A journal checkpoint turns the journal's typed row edits into Prolly data and index trees. The data tree was already updated incrementally with `prolly.Apply`. The index trees were not: `checkpointIndexTrees` called `rebuildIndexesFromTree`, which:

- read every entry of the new data tree;
- decoded every row and encoded every index key;
- sorted them all and built each index with `prolly.Build`.

That cost O(table) CPU and memory per checkpoint, even when only one row changed. On dbbench `mutable` at 100k rows it was 0.84 s of the 1.35 s spent in growth-round checkpoints.

Now, when the base snapshot already has index trees, the checkpoint derives just the index edits implied by the pending row edits and applies them to those trees. Index cost is O(pending edits × indexes × log N). Growth-round checkpoint CPU dropped to 0.65 s; index work is 0.18 s, including `Reachable`.

## How the pieces fit together

### `deriveIndexEdits` (engine/catalog.go)

This function was extracted from `tableState.overlayIndexEdits`, which the journal read and commit path uses to overlay pending rows onto persisted index trees. Given the base data tree and row edits in primary-key order, it does the following for each row and index:

- encodes the base row's index key (the old key) and the edited row's key (the new key);
- emits nothing if the two are equal;
- otherwise emits a removal of the old key and an addition of the new one.

Each index's removals are emitted before its additions, the rdb-9afb3c rule. When a unique value moves from row A to row B, B's addition must come after A's removal whichever row sorts first.

`overlayIndexEdits` now gathers the overlay, sorts it and calls `deriveIndexEdits`. Its behaviour is unchanged. Sharing one derivation means the checkpoint and the read path cannot disagree about which index entries a row edit implies.

### Checkpoint fast path (engine/engine.go)

`checkpointIndexTrees` gained an optional `*indexDelta` (base store, base table, base data tree, pending rows). Only the branch for tables with pending edits and an existing data root passes one. `indexDelta.usable` gates the fast path. All of the following must hold:

- the schema root equals the base's, so the index definitions are identical;
- the data root equals the base's, so the edits were made against this tree;
- every index has a valid root in the base manifest.

When it holds, `applyIndexEdits` runs:

1. If the new data tree is empty (`Count` reads only the root), it returns no roots. `rebuildIndexesFromTree` does the same for an empty table, and the manifest shape must match.
2. It decodes the pending rows and calls `deriveIndexEdits` against the base data tree.
3. `orderIndexEdits` stable-sorts each index's edits by key and collapses repeats; the later edit wins. Because removals precede additions, an addition always overrides a removal of the same key. Two additions of the same key would mean a duplicate unique value. That returns an error, mirroring the full rebuild's duplicate check, instead of silently picking one.
4. `prolly.Apply` writes each index's edits onto its base tree.

When the gate fails, the full rebuild runs as before:

- the first checkpoint of a table, since a base built from an empty table has no index roots;
- any schema change, such as CREATE INDEX;
- the non-pending schema-change branch;
- merge.

## Why byte-identical results matter

`prolly.Apply` preserves `Build`'s content-defined chunking, so an incrementally updated index has exactly the same root hash as one rebuilt from scratch. That makes the property test strict.

`TestCheckpointIndexEditsMatchRebuild` is in the internal package so it can call `rebuildIndexesFromTree`. Its table has a unique index, a non-unique index and a composite index. It runs 40 random rounds covering:

- inserts and updates, including NULLs;
- unique values moved between rows;
- single-row, ranged and whole-table deletes.

After each checkpoint, every index root must equal a rebuild into a memory store. Index/data divergence was the main risk, and this test catches it: making repeated keys keep the first edit instead of the last made it fail.

## Non-obvious points

- The fast path reads the **base** index trees from `base.Store()` and writes through the snapshot writer, the same as the data tree path.
- Pending row keys are copied before use, because a `PendingRows` iterator may share its buffers.
- `Reachable` still walks each new index tree to build the retain set, which is O(index nodes). At 100k rows it costs 0.04 s, so it was left alone. The data tree path pays the same cost.
- Measurements were taken at 100k rows only. The scaling claim rests on the code path no longer touching unchanged rows.
