# Journal replay: lazy base and deferred transactions

## What

On a cold cache, `WorkingState.load` replayed the journal from offset 0 and did eager work for every record:
- `repo.SnapshotCommit` (git `ls-tree -r` + manifest read + validation) for the initial base, every `checkpoint`, and every `commit` whose prepare had a new base;
- applying every committed transaction's edits.

All but the final state were discarded. Make replay do only the work that contributes to the final view.

## Why

This is the dominant journal-sync cost in the rdb-6ae82d scorecard. Replay was O(history), and a sequence of syncs quadratic: divergent merge at 1k rows went 3.6 s → 14.8 s over 30 syncs, while native-git stayed flat. The probe showed the snapshots of all historical data commits being re-listed on every sync.

## How (revised during implementation)

A lazy base alone was not enough. The journal pattern "transaction, checkpoint, transaction, checkpoint…" means every transaction after a checkpoint is applied to a new base, which forced that base to load: still 21 loads for 20 checkpoints. The key observation is that **a checkpoint supersedes every transaction before it**. So:

- **Lazy base.** The initial base, `checkpoint` records, and base changes on `commit` set `view.baseCommit` and mark the base unloaded. `ensureBase()` loads `SnapshotCommit(view.baseCommit)` only when needed, and sets its generation as the eager code did.
- **Deferred transactions.** Each `commit` is fully validated when read: matching prepare, monotonic generation, "base changed while dirty", and for legacy `prepare` the manifest inventory (prepare records' object checksums are still verified when read). The prepare is then appended to `deferred` instead of being applied. A `checkpoint` or a base change clears `deferred`.
- **Materialize.** `materialize()` = `ensureBase()` + apply `deferred` in order. It runs before `reconcileHead` when the view is dirty (reconcile compares the dirty snapshot's manifest with the head), and once at the end. When a clean view's base differs from the head, `reconcileHead` loads the head itself, so the stale base is never loaded.
- New metric `WorkingMetrics.SnapshotLoads`: snapshots loaded by replay and reconciliation, via a counting `snapshotCommit` helper.

Result: a cold replay loads at most one snapshot per distinct base that has transactions after the last checkpoint, plus the head when reconciliation needs it. That's exactly one in the common cases.

## Decisions

- **Intermediate checkpoints and superseded transactions are not re-materialized on replay.** They don't contribute to the current state. Their records are still structurally validated in order, so journal corruption is detected as before. An unreadable *old* checkpoint commit (e.g. after aggressive GC) no longer breaks replay.
- **Compaction stays out of scope** (rdb-515fae). Replay still reads every frame, but frames are cheap; snapshot loads were the cost.

## Acceptance criteria (met)

- `TestJournalReplayLoadsBoundedSnapshots`: 20 checkpoints then a cold replay. The clean, dirty-tail and base-change cases each load ≤ 1 snapshot, down from 21–22 before the fix, and rows are correct after reopening.
- All existing tests pass (working-state corruption and recovery included), plus race tests and `make lint`.
- Probe (journal, 1k rows, 10 syncs): distinct ls-tree commits per push are flat (3/4, before 2 → 22). Divergent merge goes 3.3 → 9.6 s per sync before, 3.4 → 2.9 s after (total 65.3 → 31.8 s); conflict resolve 80.0 → 44.0 s.
