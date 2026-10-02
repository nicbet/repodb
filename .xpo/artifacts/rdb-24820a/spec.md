# Secondary-index overlay costs O(own edits)

## What

Each secondary index's pending edits become a shared, immutable `repository.PendingRows` base plus the transaction's own appended edits. Index scans, unique checks, statement undo and journal commits then cost O(log P) or O(own edits), not O(P_index). This finishes for indexes what rdb-a6a4d2 did for rows.

## Why

At P = 10,000 a unique-index lookup takes 982 µs (48 µs at P = 0), and an `UPDATE` of an indexed column takes 5.1 ms (0.57 ms). See the issue for the four causes.

## Acceptance Criteria

- Unique and non-unique index lookups, `INSERT` into a table with a unique index, and `UPDATE` of an indexed column at P = 10,000 are within 1.5× of P = 0 (probe, recorded in the issue).
- Index results, unique enforcement and statement rollback are unchanged. The existing journal-index and range suites pass, and new tests cover:
  - a failed statement's index edits being undone over a pending base;
  - unique violations against pending-only keys;
  - index scans over base plus local edits, including a key deleted locally that exists only in the base.
- `make test` and `make lint` pass.

## Flow

1. **Type (`engine/catalog.go`):** `indexOverlay{base repository.PendingRows; local []repository.TypedRowEdit}`. `tableState.idxEdits` becomes `map[string]*indexOverlay`. Methods:
   - `add(edit)` appends to `local`;
   - `get(key)` scans `local` backwards, then `base.Get`;
   - `view()` returns `base.With(local)`, or `base` itself when `local` is empty;
   - `sorted()` returns `view()` as a key-ordered `[]prolly.Edit`.
2. **Cache:** `database.indexEdits` becomes `map[table]map[index]repository.PendingRows`.
   - `StartTransaction` wraps each cached set in a fresh `indexOverlay`.
   - `storeIndexEdits` caches `overlay.view()`.
   - `rememberIndexEdits` caches `base` for overlays without local edits.
   - `compactIndexEdits` and `shareIndexEdits` go.
3. **`ensureIndexEdits`:** an index built from the row overlay (`overlayIndexEdits`) or from all rows (an unpersisted index) becomes the `base` via `PendingRows{}.With(built)`. That is O(P log P) once per generation, then cached as before.
4. **Reads:** `lookupIndexKey` uses `get`. `newIndexRowIter` iterates `view().Iter(iv.start, iv.end)` through a new `pendingCursor` (`overlayCursor` over a `PendingIter`) whose current edit gives the primary key. No map, no sort.
5. **Statement undo:** `editor.idxSnapshot` records `len(local)` per index. `DiscardChanges` truncates `local` back to it, and drops overlays that `ensureIndexEdits` created during the statement.
6. **Native-git commit:** `commitIndexTrees` uses `sorted()`. Since the journal is clean in native-git mode, that is just the local edits. `coalesceEdits` goes.
7. **DDL paths** keep setting `idxEdits = nil`. `RenameIndex` and `DropIndex` move or delete map entries as today.

## Decisions

- **Reuse `PendingRows`, not a second structure.** An index edit has the same shape as a row edit (key, value = primary key, delete) and the same newest-wins semantics. Index edits live in the engine package as `[]repository.TypedRowEdit`, converted from `prolly.Edit` once on `add`.
- **Undo by truncation.** Local edits are append-only within a statement, so restoring a length replaces copying every edit.
- **`view()` per scan.** It merges the transaction's few local edits into a new run (O(L log L)). With no local edits it's free.

## Edge Cases

- MEDIUM: `ensureIndexEdits` can run inside `StatementBegin`, creating overlays mid-transaction. Undo must restore exactly the state at `StatementBegin`. It records lengths after `ensureIndexEdits` runs there, so a `DiscardChanges` truncation is exact.
- MEDIUM: Unique checks consult `get` and then the persisted tree (`lookupIndexKey`). A base delete must hide a tree key, so `get` reports deletes, as today.
- LOW: A local edit list can grow large in a big transaction (bulk insert). `view()` per scan is then O(L log L), the same as today's per-scan sort, but only over the transaction's own edits.

## Assumptions

- The per-generation rebuild in `ensureIndexEdits` on a cache miss stays O(P). It runs once per engine and generation, for example after another process commits, like the row overlay before rdb-a6a4d2 removed its eager decode. Making it incremental across processes is out of scope.
