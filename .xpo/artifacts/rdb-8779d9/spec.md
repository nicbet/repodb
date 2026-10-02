# Spec: Incremental secondary-index checkpoint

## What

When a journal checkpoint publishes a table with pending row edits, update each secondary-index tree by applying the index edits those row edits imply to the base snapshot's index tree with `prolly.Apply`. Today the checkpoint rebuilds every index from the whole data tree. The new cost is O(pending edits × indexes × log N), not O(table).

## Why

Profiling rdb-93103f (dbbench `mutable`, 100k issues) put `rebuildIndexesFromTree` at about 0.27 s per checkpoint, roughly 10% of a round. The cost grows with table size even when a single row changed, and memory is O(table).

## How

1. **Shared derivation.** Extract the core of `tableState.overlayIndexEdits` (`engine/catalog.go`) into a free function:
   `deriveIndexEdits(ctx, schema, indexes, baseData *prolly.Tree, visit) (map[string][]prolly.Edit, error)`.
   `visit` yields `(pk, delete, row)` in key order. For each row, the function looks up the base row, encodes its old and new index keys, and skips unchanged keys. Each index gets its removals first, then its additions (the rdb-9afb3c ordering). `overlayIndexEdits` keeps its behaviour and becomes a thin caller.
2. **Checkpoint fast path** (`engine/engine.go`, the pending-edits branch with a valid base `DataRoot`). Use the incremental path only when all of these hold:
   - the table's schema root is unchanged from the base;
   - the schema has indexes;
   - the base manifest has a valid root for every index.

   Under those conditions:
   - decode the pending rows and call `deriveIndexEdits` against the base data tree;
   - normalise each index's edits into strictly ordered keys with a stable sort. The last edit per key wins, so an addition overrides a removal. Two *additions* of the same key are an error, mirroring the full rebuild's duplicate check;
   - `prolly.Apply` the edits to the base index tree, then collect its reachable hashes.

   If the new data tree is empty, return no index roots, as the full rebuild does today.
3. **Fallback.** Every other case still calls `rebuildIndexesFromTree`: schema changed (for example CREATE INDEX), no base data root, or a missing base index root. Merge (`engine/merge.go`) is unchanged.

`prolly.Apply` keeps Build's canonical chunking, so the incremental result must be **byte-identical** (same root hash) to a full rebuild.

## Edge cases

- A unique value moves from row A to row B in one checkpoint window: B's addition wins over A's removal (stable sort, last wins).
- Row deleted: removal only. Row inserted: addition only.
- Row updated without changing indexed columns: no index edit.
- NULL in a unique index: the key carries the PK suffix, handled by `encodeIndexKey`.
- All rows deleted: the data tree is empty, so the table gets no index roots (as today).

## Acceptance criteria

- [ ] A checkpoint of a table with pending edits, an unchanged schema and persisted index roots never calls `rebuildIndexesFromTree` or `Tree.Entries`.
- [ ] A property test runs random insert, update and delete sequences, including unique-value moves, NULLs and full deletion, across several checkpoints. After each checkpoint, every index root equals the root `rebuildIndexesFromTree` produces from the checkpointed data tree.
- [ ] Existing engine and journal tests pass.
- [ ] Profiling the dbbench `mutable` round (in the Linux container) shows index checkpoint cost no longer scaling with table size. Report the before and after figures in the completion comment.

## Agent Decisions

- **Fallback keeps the full rebuild for schema changes.** Reusing roots of unchanged indexes after an ALTER is possible, but schema changes are rare and the rebuild is the existing, correct path. Revisit if DDL-heavy workloads show up.
- **Duplicate additions error instead of silently picking one.** That case means a UNIQUE violation slipped past commit-time checks. Failing the checkpoint surfaces it the same way the full rebuild does.
- **No docs change.** Behaviour and storage are identical, since roots are byte-identical. Only the cost changes.
