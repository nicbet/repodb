# rdb-e0c717: Head advances under a dirty journal wedge working state

## What

The data head (`refs/repodb/data`) can move while the journal has uncheckpointed transactions. Every later fresh load then fails with `ErrWorkingBaseChanged`:
- journal engines can't checkpoint (the writer's expected head is stale, so they get `ErrConflict` every time);
- native-git engines refuse to open;
- no command recovers.

Acknowledged writes are durable but stranded.

## Why (by code reading; reproduced by tests first)

Two in-process paths move the head without holding `working.lock`:

1. **Sync / Enable.** `requireCleanWorkingState` takes `working.lock` only for the check. Fetch, snapshot validation (seconds at 50k rows) and the head update (`FastForwardSnapshot` or the merge `Writer.CommitWithOutcome`) run afterwards, holding only `publish.lock`. A journal commit that lands in the gap records the old head as its base. `Enable`'s adopt-remote fast-forward has the same gap.
2. **Native-git commits** (found while investigating). `engine.Open` checks that the journal is clean only at open. A journal-mode engine writing afterwards makes the journal dirty, and the native engine's next commit moves the head under it.

A third path is out of RepoDB's control: an external `git update-ref`.

In-process, the journal engine doesn't notice. A cached dirty view with no new frames is returned without checking the head (`load`), so it keeps committing on the stale base.

## How

### Lock order

All head advances follow one order: **`working.lock` → `publish.lock`**. That is what `Checkpoint` already does.

### Repository layer (`common/repository`)

- `Repository` gets a lazily created guard: a `*WorkingState` used only for publication checks. It has its own small cache for a "journal dirty?" scan: file identity, offset and dirty flag.
- `journalDirtyLocked()` (caller holds `working.lock`):
  - no journal file → clean;
  - otherwise read frames from the cached offset: a `commit` marker sets dirty, a `checkpoint` clears it, and incomplete prepares don't count;
  - no snapshot loads, no head resolution, no Git subprocess.
- `lockForPublication(ctx, requireClean bool)` acquires `working.lock`, then `publish.lock`. When `requireClean` is set and the journal is dirty, it releases both and returns `ErrWorkingStateDirty`, wrapped with a hint to checkpoint first.
- `Writer.CommitWithOutcomeMessage` uses `lockForPublication(ctx, true)`, unless the writer is marked `workingLockHeld`. `Checkpoint` and `CheckpointPrepared` set that mark: they already hold `working.lock`, and they are the one sanctioned way to publish a dirty journal.
- `WithPublicationLock` takes `working.lock` → `publish.lock`. `LockedPublication.FastForward`/`FastForwardSnapshot` check the journal under those locks and refuse with `ErrWorkingStateDirty`. `LockedPublication.Head` (used by `pushExpected`) doesn't require clean. `pushExpected` pushes after releasing the locks, as before.
- **In-process re-entrancy.** File locks on separate descriptors block each other within one process. No path takes `working.lock` twice; the `workingLockHeld` mark covers the checkpoint path.

### Sync and Enable

No new locking code. Their head updates go through the guarded publication paths:
- a journal commit that won the race makes sync fail. `ErrWorkingStateDirty` is mapped to `ErrWorkingDirty` (action `working-dirty`);
- a journal commit attempted while sync publishes waits for `working.lock`, then sees the new head through `reconcileHead` (the journal is clean, so it re-bases).

The early `requireCleanWorkingState` checks stay as fast-fail before the expensive fetch and validation.

### Native-git engines

A commit while the journal is dirty fails with `ErrWorkingStateDirty` (outcome rejected; retryable after a checkpoint). Cost: one `working.lock` acquisition and a cached scan of new journal frames per native commit. Without a journal file it's one `stat`.

### Recovery for already-wedged repositories (decision: documented reset)

- `ErrWorkingBaseChanged` is wrapped as `…: journal base <b>, data head <h>; see "Recovering a stranded journal" in docs/cli.md`.
- `docs/cli.md#recovering-a-stranded-journal`:
  1. keep `<head>` if it's local-only (it's also in the reflog);
  2. `git update-ref refs/repodb/data <base>`;
  3. `repodb commit -m …`;
  4. `repodb sync`, which merges through the usual three-way merge.

## Decisions

- **Publication-layer guard rather than patching sync only** (user decision). It covers sync, enable and native-git commits with one rule.
- **Documented reset, not a recovery command** (user decision).
- **External `git update-ref` under a dirty journal stays possible.** It is detected on the next fresh load and recovered with the documented procedure. Journal commits don't re-resolve the head per transaction: that would put Git ref I/O on the journal fast path, and in-process head moves are now impossible while dirty.

## Bug found and fixed here: rdb-ed0738

After any rejected `COMMIT` (including plain `ErrConflict`), the session kept the dead transaction: it saw its own unpublished rows, and a retry failed with a duplicate key or `snapshot writer is already committed`. go-mysql-server clears the transaction only after a successful commit. `session.CommitTransaction` now calls `ctx.SetTransaction(nil)` and `SetIgnoreAutoCommit(false)` on error, as MySQL rolls back a failed commit. This was needed for this issue's "retry after checkpoint" acceptance.

## Tests (as built)

1. `integration/journal_race_test.go` `TestSyncRejectsJournalCommitRacingPublication`, fast-forward and merge: a journal commit through the `beforePublish` hook (unexported, set via `export_test.go`). Asserts:
   - sync returns `ErrWorkingDirty`;
   - the ref is unchanged;
   - a fresh status is dirty and loadable;
   - after a checkpoint, sync succeeds with all rows.
2. `engine` `TestNativeGitCommitRejectedWhileJournalDirty`: the native commit is rejected and the head unchanged; after a checkpoint the retry succeeds.
3. `engine` `TestConcurrentJournalAndNativeCommitsKeepWorkingStateLoadable`: journal commits, checkpoints and native commits race. No deadlock (60s timeout), and the working state is loadable and clean at the end. Also passes under `-race`. Sync's race is covered deterministically by test 1.
4. `integration` `TestStrandedJournalRecoveryProcedure`: wedge through `git update-ref`. The error names base, head and the doc section, and the documented steps recover every row.
5. `engine` `TestRejectedCommitStartsFreshTransaction` (rdb-ed0738), native-git and journal.

Before the fix, tests 1 and 2 failed as described in "Why". Test 5 failed without the `CommitTransaction` change.

**Cost.** `BenchmarkSQLWriteBatches` (native-git) is unchanged within noise, before vs after (3 runs each).

## Docs

- `docs/architecture.md`: the lock table, the native-git publication step, the journal guards, concurrency limits, and the sync fast-forward and merge publication.
- `docs/cli.md`: sync's behavior when a journal commit races it, and "Recovering a stranded journal".
- `docs/library.md`: native-git commits rejected under a dirty journal; `Sync` racing a journal commit.
- `docs/sql.md`: a failed `COMMIT` ends the transaction.
- `docs/testing.md`: coverage added; the race gap removed.

## Acceptance

- A journal commit racing sync or a native-git commit leaves the repository consistent: one side is rejected, never `ErrWorkingBaseChanged`.
- A wedged repository recovers with the documented procedure, proven by test 4.
- `make test` and `make lint` pass.
