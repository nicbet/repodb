# rdb-53214c walkthrough: Diff lists journal row edits

## The bug

In journal mode, RepoDB doesn't rebuild a table's Prolly tree on every transaction. Each committed transaction appends a `typed-prepare` record of row edits. On load, `applyTypedEditsToSnapshot` folds those records into `workingView.pendingEdits` (table name → encoded key → `TypedRowEdit`), and reads overlay them on the base tree. The working manifest changes only when a table's schema changes (new `SchemaRoot`) or the table is dropped. The `DataRoot` stays at the checkpoint's value until the next `Checkpoint` materializes the rows.

`WorkingState.Diff` compared the base and working manifests entry by entry (`before.Equal(after)`). A table with only row edits had identical entries, so it was skipped. `repodb diff` printed "No uncommitted data changes." while `repodb status` said the journal was dirty.

## The fix

`Diff` now skips an unchanged manifest entry only when the table also has no pending row edits:

```go
case before.Equal(after) && !hasPendingRows(view.pendingEdits[name]):
    continue
```

No separate pass over `pendingEdits` is needed. `Diff` already iterates over the union of base and working table names, and every table with pending rows is in the working manifest:

- A table created since the checkpoint gets a manifest entry from its schema edit, so it is already reported as `added`.
- A dropped table is removed from both the manifest and `pendingEdits`, so stale row edits can't resurrect it.

## Decisions

- **Edits that cancel out still count.** An insert followed by a delete of the same new key leaves a pending delete, so the table is `modified`. Status is dirty in the same case, so the two commands agree. Detecting net no-ops would need a base lookup per pending key. Approved during review.
- **No per-table row counts.** Telling an insert from an update needs the same base lookup. `TableChange` keeps its `{Table, Change}` shape. Approved during review.

## Tests

`TestJournalDiffListsRowOnlyChanges` (`engine/working_test.go`), journal mode:

- empty after a checkpoint;
- insert-only, update-only and delete-only changes, each after a fresh checkpoint → `[{t modified}]`;
- a native-git engine writing to the same repository leaves `Diff` empty;
- a new table with rows plus an edit to an existing one → `[{t modified} {u added}]`;
- after closing the engine, a fresh `repository.OpenWorkingState` replays the journal from disk and reports the same, with `Status` dirty.

Also checked by hand with the built CLI: after a journal-mode `INSERT`, `repodb status` reported dirty and `repodb diff` printed `modified	t`.

## Docs

`docs/cli.md`, `docs/library.md` and `docs/testing.md` no longer cite the limitation and describe the current behavior, including edits that cancel out.
