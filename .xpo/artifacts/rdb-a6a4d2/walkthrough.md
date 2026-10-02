# Walkthrough: journal overlay costs O(own edits), not O(pending edits)

## What was built and why

In journal mode, row edits since the last checkpoint ("pending edits", P) live only in the journal and are overlaid on the checkpointed Prolly trees when reading. Every transaction paid for all P of them:

- `StartTransaction` copied the decoded overlay into the transaction's `tableState.edits` map.
- The transaction then wrote its own edits into that same map, so `commitTypedEdits` re-appended **every** pending edit to the journal. At P = 10,000 a single-row `UPDATE` appended 856 KB, and the journal grew quadratically between checkpoints.
- Each scan sorted all pending keys, and each new generation re-decoded all P rows.

Replay already merged `typed-prepare` records key by key, so the repeated edits were redundant. Removing them needed no journal format change, and journals written before the fix still replay correctly.

## How the pieces fit

### `repository.PendingRows` (`common/repository/pending.go`)

An immutable set of `TypedRowEdit`s, held as sorted runs, newest first.

- `With(edits)` builds a new run, last write per key wins. It then merges runs size-tiered: while the newest run is at least half the size of the next older one, the two merge, newest winning. Each edit is rewritten O(log P) times, and there are O(log P) runs.
- `Get` binary-searches the runs newest first. `Iter(start, end)` is a pull iterator that merges the runs in key order.
- Runs are never modified after they're built, so a snapshot, an older view and a transaction can all share one `PendingRows` without copying.

`applyTypedEditsToSnapshot` now copies only the small per-table entry struct of the tables a commit touches, and calls `With`. `Snapshot.PendingEdits()` returns `map[string]PendingRows`.

### Engine `tableState`

- `pending repository.PendingRows`: shared, read-only, values still encoded.
- `edits map[string]rowEdit`: this transaction's own decoded edits, which win over `pending`.

Helpers hide the two layers:
- `edit(key)` looks at local edits, then decodes the pending one on demand.
- `forEachEdit` visits the merged view (`ensureRows`, native-git commit, the index overlay).
- `hasEdits`.
- `overlayDeletes` serves `rekeyRows`, the only consumer that needed the old full map, and it only used the deletes.

Scans: `mergedEntries` now takes an `overlayCursor`. `rowOverlayCursor` merges the sorted local keys with `PendingRows.Iter` and hands the current edit to `primaryRowIter`. Secondary-index scans use a plain `keyCursor` over their own overlay; that path is rdb-24820a.

`commitTypedEdits` writes only `state.edits`. DDL that rewrites a table (`AddColumn`, `DropColumn`, `ModifyColumn`, PK changes) already materializes every row into `edits`, so it clears `pending`, and that commit writes the whole table as before. The `decodedEdits` cache is gone: nothing is decoded eagerly.

## Key decisions

- **Sorted runs, not copy-on-write maps or a third-party B-tree.** Any copy is O(P) per commit. A persistent B-tree would also work, but runs are about 150 lines with no dependency, and scans and checkpoints need sorted order anyway.
- **Keep pending rows encoded.** A scan decodes what it returns either way, and a point read decodes one row. This removed the per-generation re-decode of all P rows.
- **Undo works on local edits only.** `editor.remember` captures presence in the local map. Restoring it re-exposes the pending row underneath, which is the right undo.
- **`overlayDeletes` decodes nothing.** `ModifyColumn` changes the schema before re-keying, so decoding pending rows at that point failed. `rekeyRows` only ever used the delete edits.

## Measured (50k-row table, journal)

| P | `UPDATE` | Journal B/`UPDATE` | Point read |
|---:|---:|---:|---:|
| 0 | 0.40 → 0.38 ms | 419 → 419 | 87 → 83 µs |
| 10,000 | 6.79 → 0.37 ms | 856 KB → 419 | 432 → 77 µs |

## Tests

- `pending_test.go`: a random model test (newest wins, deletes, earlier versions unchanged after `With`, run count bound), last-in-batch wins, and interval iteration.
- `TestJournalCommitAppendsOnlyOwnEdits`: record size independent of 2,000 pending edits, a pending-only delete reaching the journal, then replay and checkpoint.

## Non-obvious

- `WorkingState.PendingTableEdits` was removed: it had no callers and returned an unexported type. So was `pendingTableEdits.dropped`, which was only ever copied.
- Secondary-index scans still rebuild their overlay per query (rdb-24820a).
