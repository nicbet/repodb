# rdb-53214c: Diff misses journal row edits

## What

In journal mode, `WorkingState.Diff` (and so `repodb diff`) returns nothing after row-only writes to a table that existed at the last checkpoint, while `Status` reports `Dirty=true`.

## Why

Journal transactions commit `typed-prepare` records. `applyTypedEditsToSnapshot` keeps row edits in `view.pendingEdits` (table name → key → `TypedRowEdit`) beside the base manifest. It changes a manifest entry only for a schema change (`SchemaRoot`) or a drop, so `DataRoot` stays at the base value. `Diff` compares only manifest entries (`before.Equal(after)`), so these tables look unchanged.

## How

In `Diff` (`common/repository/working.go`), a table whose base and working manifest entries are equal is skipped only if it has no pending row edits (`hasPendingRows`). `Diff` already iterates over the union of base and working table names. A table with pending row edits is always in the working manifest: a new table gets its schema entry, and a drop removes the table from both the manifest and `pendingEdits`. So this one condition covers both `modified` and `added`, with no separate loop and no de-duplication.

`TableChange` keeps its shape (`Table`, `Change`). Per-table insert/update/delete counts are out of scope: classifying an edit as insert or update needs a base lookup per key.

## Decisions

- **Net no-op edits count as modified.** Inserting a new key and deleting it again leaves a pending delete edit, so the table is reported `modified`. `Status` reports dirty in the same case, so `diff` and `status` agree. Comparing every pending edit against the base to detect net no-ops costs a lookup per key, and the case is rare. Approved by the user.
- **No counts.** Approved by the user.

## Tests (`engine/working_test.go`, `TestJournalDiffListsRowOnlyChanges`)

- After a checkpoint, `Diff` is empty.
- `INSERT`-only, `UPDATE`-only and `DELETE`-only changes, each after a fresh checkpoint → `[{t modified}]`.
- A native-git write on the same repository → `Diff` stays empty.
- A new table with rows, plus a row edit to an existing table → `[{t modified} {u added}]`, each listed once.
- After closing the engine, a fresh `repository.OpenWorkingState` (journal replay from disk) reports the same and `Status` is dirty.

Also checked by hand with the built CLI (`repodb start --persistence journal`, `sql`, `commit`, `sql`): `repodb status` reports dirty and `repodb diff` prints `modified	t`.

## Docs

- `docs/cli.md` (`repodb diff`): limitation removed; describes row-only and net no-op behavior.
- `docs/library.md`: `Diff(ctx)` describes `TableChange` and row-only changes.
- `docs/testing.md`: "Row-level diff" gap removed; journal coverage added.

## Acceptance

- Row-only changes to an existing table (insert, update, delete) are listed by `Diff`.
- `repodb diff` agrees with `repodb status` about whether anything is uncommitted.
