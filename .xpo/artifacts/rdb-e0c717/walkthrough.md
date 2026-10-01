# rdb-e0c717 walkthrough: the data head never moves under a dirty journal

## The invariant

In journal mode, committed transactions sit in the journal as edits on top of a **base commit**, the data head at the time. Until a checkpoint publishes them, that base must stay the head. If anything else moves `refs/repodb/data`:
- `reconcileHead` sees a dirty view whose base isn't the head and fails with `ErrWorkingBaseChanged` on every fresh load;
- the checkpoint's writer expects the old head and fails its compare-and-swap with `ErrConflict` forever.

The writes are durable but stranded.

Before this change, two in-process paths could break the invariant:

1. **Sync / Enable.** `requireCleanWorkingState` took `working.lock` only for the check. Fetch and validation (seconds on large tables) and the head update ran afterwards, holding only `publish.lock`. A journal commit landing in that window was based on the old head.
2. **Native-git engines.** They check the journal only at open. If a journal engine writes afterwards, the next native commit moved the head under it.

The journal engine itself didn't notice. A cached dirty view with no new frames is returned without consulting the head, so it kept accepting commits on the stale base.

## The rule: one lock order, one exception

Every update of the data head now takes **`working.lock` → `publish.lock`** and holds both through the ref update. That is the order `Checkpoint` already used. Under `working.lock`, a publication refuses with `ErrWorkingStateDirty` while the journal is dirty. The one exception is the checkpoint, the sanctioned way to publish a dirty journal.

Holding `working.lock` does two things:
- the dirty check can't go stale before the ref moves;
- a journal commit that arrives meanwhile waits. Once it gets the lock, it sees a clean journal on a new head, and `reconcileHead` re-bases it.

### Where it lives (`common/repository`)

- `Repository.lockForPublication(ctx, requireClean)` takes both locks in order. It uses a lazily created guard `WorkingState` owned by the repository, so it has its own scan cache.
- `WorkingState.journalDirtyLocked()` decides dirtiness from record kinds alone: `commit` sets dirty, `checkpoint` clears it. It caches file identity and offset, so an unchanged journal costs one `stat`, and it never loads snapshots or resolves the head. That's what makes it cheap enough for every native-git commit. `BenchmarkSQLWriteBatches` is unchanged.
- `Writer.CommitWithOutcomeMessage` calls `lockForPublication(ctx, true)`, unless `workingLockHeld` is set. `Checkpoint` and `CheckpointPrepared` set it because they already hold `working.lock`.
- `WithPublicationLock` takes both locks. `FastForwardSnapshot` performs the clean check. `pushExpected` only reads the head under these locks and pushes after releasing them, so a slow push still doesn't block SQL commits.

### Why `workingLockHeld` matters

File locks taken through different descriptors exclude each other even within one process. Without the mark, a checkpoint would deadlock against itself: it holds `working.lock` through the engine's `WorkingState`, then the writer tries to take it again through the repository's guard. No path takes `working.lock` twice now.

### Sync

Sync needed no new locking. Its fast-forward and merge publications go through the guarded paths, and it maps `ErrWorkingStateDirty` to its own `ErrWorkingDirty` (action `working-dirty`), whose advice is checkpoint and sync again. The early clean checks remain as a fast fail before the expensive fetch. A `beforePublish` hook (unexported, set from `export_test.go`) lets tests put a journal commit exactly in the window.

## Recovery

A head moved by something outside RepoDB (`git update-ref`) is still possible, and journal commits don't re-resolve the head; that would add Git ref I/O to the journal fast path. Such repositories get a clear error naming the journal base and the head, plus `docs/cli.md#recovering-a-stranded-journal`:
1. put the ref back on the base;
2. checkpoint;
3. sync, whose three-way merge reconciles the stranded transactions with the remote.

The user chose this over a dedicated command. `TestStrandedJournalRecoveryProcedure` proves the steps recover every row.

## rdb-ed0738, fixed on the way

The native-git test's "retry after checkpoint" step exposed an older bug. go-mysql-server clears the session's transaction only after a **successful** commit, but a RepoDB transaction's writer is single-use. After any rejected `COMMIT` (even a plain `ErrConflict`), the session sat on a dead transaction: it saw its own unpublished rows, and retries failed. `session.CommitTransaction` now clears the transaction and explicit-transaction mode on error, as MySQL rolls back a failed commit. `TestRejectedCommitStartsFreshTransaction` covers both modes.

## Tests

| Test | Proves |
| --- | --- |
| `TestSyncRejectsJournalCommitRacingPublication` (fast-forward, merge) | a journal commit in the window makes sync fail with the ref unchanged; checkpoint + sync then merges everything |
| `TestNativeGitCommitRejectedWhileJournalDirty` | native-git commit rejected, head unchanged, retry after checkpoint succeeds |
| `TestConcurrentJournalAndNativeCommitsKeepWorkingStateLoadable` | racing writers and checkpoints neither deadlock nor strand the journal (also under `-race`) |
| `TestStrandedJournalRecoveryProcedure` | the error message and the documented recovery |
| `TestRejectedCommitStartsFreshTransaction` | rdb-ed0738 |

Before the fix, the first two failed exactly as the issue predicted. The last failed without the `CommitTransaction` change.
