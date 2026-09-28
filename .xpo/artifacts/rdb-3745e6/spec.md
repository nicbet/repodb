# `repodb commit` drops journal row edits

## What

`repodb commit` and the "checkpoint before sync?" prompt in `repodb sync` lose every row written in journal mode since the last checkpoint. Table definitions survive; rows don't.

## Why it happens

Journal transactions record *typed row edits* (encoded key/value per row). Only the engine can materialize them into Prolly data and index trees, because that needs table schemas: `Engine.Checkpoint` → `checkpointTypedEdits` → `WorkingState.CheckpointPrepared`.

`WorkingState.Checkpoint` predates typed edits. It publishes the working snapshot's objects, which contain schemas but not the pending rows, then appends a checkpoint marker that makes the journal clean. The CLI calls it directly (`cmd/repodb/main.go` commit and `checkpointBeforeSync`), so the row edits are discarded.

## How

1. **CLI:** `commit` and `checkpointBeforeSync` checkpoint through the engine: `engine.OpenWithOptions(ctx, path, Options{Persistence: PersistenceJournal})`, then `Checkpoint`, then `Close`. Stop discarding the `OpenWorkingState` error.
2. **Guard in `WorkingState.Checkpoint`:** if the working view has pending typed row edits, return `OutcomeRejected` with an error saying to checkpoint through the engine, without publishing anything or appending a marker. It can then no longer silently drop data for any caller. Schema-only journal changes (no row edits) keep working, since the snapshot objects fully describe them.
3. **Benchmark:** `engine/sql_bench_test.go` M4.3 checkpoint benchmark switches to `eng.Checkpoint`. It currently times the data-dropping path.
4. **Tests:**
   - repository/engine: `WorkingState.Checkpoint` with pending row edits returns an error and leaves the journal dirty and the rows intact.
   - CLI regression: a Go test drives the `repodb` command's `run` entry point: journal writes → `commit` → rows present after reopen.

## Decisions

- **Guard, not materialize, in the repository layer.** The repository package has no schema knowledge; moving tree building there would invert the layering. Rejecting is enough, because the engine is the one correct path.
- **The CLI opens a full engine for checkpoint.** It costs a snapshot validation, but it's the same code path the server and embedded users use.
- **Benchmark docs not regenerated here.** `docs/m4.3-bench.md` numbers for the checkpoint benchmark were measured on the dropping path. Flag it to the user; re-running benchmarks is separate work.

## Acceptance criteria

- The repro from the issue keeps all rows after `repodb commit`, and after `repodb sync`'s checkpoint prompt.
- `WorkingState.Checkpoint` refuses pending row edits and loses nothing.
- `make test` and the race tests pass.
