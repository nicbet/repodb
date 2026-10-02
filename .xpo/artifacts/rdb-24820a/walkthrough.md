# Walkthrough: secondary-index overlay costs O(own edits)

## What was built and why

rdb-a6a4d2 made row reads and journal commits independent of the number of edits since the last checkpoint (P). Secondary indexes still paid O(P_index) per statement. Each index's pending entries were one append-only `[]prolly.Edit` per transaction, and four paths walked all of it:

1. `newIndexRowIter` copied every entry into a map and sorted its keys, per query.
2. `lookupIndexKey` (unique checks on insert and update) scanned every entry backwards.
3. `editor.StatementBegin` copied every entry, to undo a failed statement.
4. `storeIndexEdits` and `rememberIndexEdits` compacted and sorted every entry, per commit.

At P = 10,000 a unique lookup took 1.0 ms (51 µs at P = 0) and an indexed `UPDATE` 5.3 ms (0.5 ms).

## How the pieces fit

`indexOverlay` (`engine/catalog.go`) is the same two-layer shape as the row overlay:

- `base repository.PendingRows`: the journal's entries for this index. Shared and immutable, cached per generation in `database.indexEdits`.
- `local []repository.TypedRowEdit`: this transaction's entries, append-only, last wins per key.

Methods:
- `add` appends.
- `get` scans `local` backwards (small), then `base.Get`.
- `view` returns `base.With(local)`, which is `base` itself when nothing is local.
- `sorted` returns the view as key-ordered `prolly.Edit`s, for native-git's `commitIndexTrees`.

Lifecycle:
- **`StartTransaction`** wraps each cached `PendingRows` in a fresh overlay: O(#indexes), no copy.
- **`ensureIndexEdits`** (cache miss: first engine of a generation, or after another process committed) derives entries from the row overlay, or from all rows for an unpersisted index. It makes them the base via `With`. That's O(P log P) once, then cached by `rememberIndexEdits`.
- **Reads:** `lookupIndexKey` uses `get`. `newIndexRowIter` takes `view()` once and walks each interval with `pendingCursor` (an `overlayCursor` over `PendingIter`). The cursor's current edit gives the primary key, or marks a delete.
- **Statement undo:** `idxSnapshot` stores `len(local)` per index, and `DiscardChanges` truncates back to it. An overlay that `addIndexEdit` created during the statement is removed.
- **Commit:** `storeIndexEdits` caches `view()` per index. `With` merges size-tiered, so the cost is amortized O(local · log P).

## Key decisions

- **Reuse `PendingRows`.** An index entry is a key, a value (the primary key) and a delete flag, with newest-wins semantics, exactly like a row edit.
- **Undo by truncation.** Local entries only grow within a statement, so a length restores them exactly.
- **`view()` per scan.** It costs O(L log L) in the transaction's own entries, the same as today's per-scan sort but no longer over P. With no local entries it's free.

## Measured (50k rows, unique and non-unique index, journal)

| P | unique lookup | non-unique lookup | `INSERT` | indexed `UPDATE` |
|---:|---:|---:|---:|---:|
| 0 | 51 → 52 µs | 127 → 132 µs | 0.56 → 0.56 ms | 0.51 → 0.45 ms |
| 10,000 | 1,019 → 44 µs | 1,174 → 110 µs | 5.50 → 0.48 ms | 5.26 → 0.39 ms |

## Tests

`TestJournalIndexOverlayLayers`:
- a unique violation against a pending-only key;
- a failed multi-row insert whose first row's index entry must be rolled back, then re-inserted;
- a local delete hiding a pending-only entry, which also frees its unique key;
- a local update moving a row into a scanned range;
- commit, and a reopened engine rebuilding the overlay.

The existing model test (`TestJournalIndexesMatchModel`) and range suites pass unchanged.

## Non-obvious

- **The cache-miss rebuild is still O(P),** once per engine and generation. Making it incremental across processes would mean persisting index entries in the journal, which is out of scope.
- **A new index has no overlay until its first edit.** `CreateIndex` inside a transaction doesn't reset `idxEdits`, so the new index gets an overlay from `addIndexEdit`. That path, and its undo, is the only case where `DiscardChanges` deletes an overlay.
