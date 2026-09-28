# Walkthrough: `repodb commit` dropped journal row edits

## The bug

In journal mode, `repodb commit -m …` (and answering "y" to `repodb sync`'s "checkpoint before syncing?" prompt) published a Git data commit with the right tables but **no rows** from the journal, then marked the journal clean. The rows were gone.

## Why

Journal transactions don't write Prolly trees. They append *typed row edits* (encoded key → encoded row, or a delete) to the journal. Turning those edits into data and index trees needs table schemas, so only the engine can do it:

```
Engine.Checkpoint
  └─ pending edits? → checkpointTypedEdits → builds trees → WorkingState.CheckpointPrepared
  └─ none?          → WorkingState.Checkpoint (publish the snapshot's objects as-is)
```

`WorkingState.Checkpoint` is the older path from before typed edits existed. It copies the working snapshot's objects into a Git commit, and those objects include schemas but not pending rows. It then appends the checkpoint marker that makes the journal clean. The CLI called it directly, skipping the engine.

## The fix

1. **CLI goes through the engine.** A new `checkpoint(ctx, repoPath, message)` helper in `cmd/repodb/main.go` opens a journal-mode engine, calls `Engine.Checkpoint`, and closes it. Both `commit` and `promptCheckpointIfDirty` use it. The sync prompt also stopped ignoring the `OpenWorkingState` error.
2. **The repository layer refuses instead of dropping.** `WorkingState.Checkpoint` now returns `ErrPendingRowEdits` (Outcome `Rejected`, nothing published, journal untouched) if any table has pending row edits. It doesn't try to build the trees itself, because the repository package has no schema knowledge. Refusing makes the silent-loss failure mode impossible for any future caller. Schema-only journal changes still checkpoint through it, because the snapshot objects fully describe them (`TestJournalRecoversCheckpointPublishedBeforeBookkeeping` relies on that).
3. **Benchmark corrected.** `BenchmarkM43JournalCheckpoint` called `WorkingState().Checkpoint` after UPDATEs, so it was timing the row-dropping path. It now calls `eng.Checkpoint`. The historical numbers recorded for it predate this fix.

## Tests

- `engine/working_test.go` `TestWorkingCheckpointRefusesPendingRowEdits`: the guard returns `ErrPendingRowEdits`, the journal stays dirty, and a following engine checkpoint keeps the rows.
- `cmd/repodb/main_test.go` `TestCommitKeepsJournalRows`: the first test for the CLI package. It drives `run(ctx, "commit", …)` end to end. Against the pre-fix CLI it fails with `ErrPendingRowEdits`, so the guard catches the old code path.

The sync prompt path isn't unit-tested (it reads stdin and needs a remote). It uses the same helper.
