# Journal overlay costs O(pending edits) per transaction

## What

Make the cost of a journal transaction depend on its own size, not on the number of uncheckpointed row edits (P). Pending row edits are held in a persistent, sorted structure that snapshots and transactions share without copying. A commit appends only the transaction's own edits.

## Why

Measured at P = 10,000 (50k-row table): a single-row `UPDATE` takes 6.8 ms and appends 856 KB to the journal (0.4 ms and 419 B at P = 0). A point read takes 432 µs (87 µs). Between checkpoints the journal grows quadratically. See the issue for causes.

## Acceptance Criteria

- Journal bytes appended by a single-row `UPDATE` are independent of P (regression test: record size at P = 2,000 within a few bytes of P = 0).
- `StartTransaction` and point reads neither copy nor decode the pending overlay; scans do not sort it.
- Single-row `UPDATE` and point read at P = 10,000 within 1.5× of P = 0 (probe benchmark, recorded in the issue).
- Existing journals replay unchanged (their records just carry redundant edits). No journal or storage format change.
- `make test` and `make lint` pass.

## Flow

1. **`common/repository/pending.go`: persistent pending rows.** `PendingRows` is an immutable list of sorted runs, newest first. Each run is a sorted slice of `TypedRowEdit` and is never modified after creation.
   - `With(edits)` returns a new `PendingRows` with the edits as a new run (sorted, last write per key wins). It then merges runs size-tiered: while a run is at least half the size of the older run, the two are merged into a new run, newest wins per key. Each edit is merged O(log P) times, amortized.
   - `Get(key)`: binary search per run, newest first.
   - `Iter(start, end)`: a pull iterator over the runs merged by key (newest wins) within `[start, end)`, with deletes surfaced as entries.
   - `Len()`: an upper bound on distinct keys (sum of run lengths), and `Empty()`.
2. **`WorkingState`**: `pendingTableEdits.rows` becomes a `PendingRows`. `applyTypedEditsToSnapshot` copies only the small per-table map, calls `With` for each edited table, and shares untouched tables. `Snapshot.PendingEdits()` returns `map[string]PendingRows`. Diff/status (`hasPendingRows`) and replay use the new type.
3. **Engine `tableState`**: `pending repository.PendingRows` (shared, read-only, encoded values) plus `edits` (this transaction's own decoded edits, as today). Helpers:
   - `edit(key)`: the local edit, else the pending one decoded on demand;
   - `overlay(iv)`: a merged, ordered iterator over pending and local edits in an interval (local wins), used by `mergedEntries`, `ensureRows`, `overlayIndexEdits` and native-git commit.
4. **`StartTransaction`** assigns `state.pending` from the snapshot. The `decodedEdits` cache and its per-transaction map copy are removed.
5. **`commitTypedEdits`** writes `state.edits` only, which are the transaction's own edits. Where DDL rebuilds a table (`AddColumn`, `ModifyColumn`, `DropColumn`, PK changes), the rebuild already materializes every row into `state.edits`. It also clears `state.pending`, so that commit writes the whole table as it does today.
6. **`Engine.Checkpoint`** (`checkpointTypedEdits`) iterates `PendingRows` in key order instead of sorting a map.
7. **Tests**: `PendingRows` unit tests (merge order, newest wins, deletes, interval iteration, sharing after `With`); an engine test that the appended record size is independent of P; the existing journal, range and merge suites as regression.

## Decisions

- **Size-tiered sorted runs, not copy-on-write maps.** Copying maps less often is still O(P) per commit. Runs are immutable, so snapshots share them, and they are already sorted for scans and checkpoints.
- **No third-party B-tree** (`tidwall/btree`, `google/btree`). A persistent B-tree would also work, but the runs are about 150 lines and add no dependency.
- **Pending rows stay encoded, and the engine decodes them on access.** A scan decodes the rows it returns either way, and a point read decodes one row. This removes the per-generation re-decode of all P rows.
- **Undo (`DiscardChanges`) works on local edits only.** Today it captures presence in the one shared map. With layering, it restores the local map, which re-exposes the pending row.

## Edge Cases

- MEDIUM: A local delete of a key that exists only in the pending overlay must reach the journal as a delete. It does, because local edits are written as-is.
- MEDIUM: DDL after pending edits. The rebuild path materializes rows through `ensureRows`, which must merge tree, pending and local edits. Covered by the existing `ALTER TABLE` journal tests.
- LOW: Replay of journals written before this change. Their `typed-prepare` records repeat earlier edits, and merging them again is harmless.
- LOW: Many tiny commits. Run count stays O(log P), and lookups cost O(log² P).

## Assumptions

- Native-git mode never has pending edits (`ErrWorkingStateDirty` guards it), so its commit path only ever sees local edits. It still iterates the merged view, to be safe.
- Index overlay caching (`indexEdits`, `storeIndexEdits`) stays as is. It already caches per generation and is outside this bug.
