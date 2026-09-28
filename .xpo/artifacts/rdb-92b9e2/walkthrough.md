# Walkthrough: journal replay does only the work that matters

## The problem

Journal-mode sync got slower with every sync. In the rdb-6ae82d scorecard, divergent merge at 1k rows went 3.6 s → 14.8 s over 30 syncs, while native-git stayed flat. That accounted for most of journal's 50-minute run.

A git-call probe showed each sync re-listing (`git ls-tree -r`) the snapshot of *every* historical data commit, several times.

## Why

`WorkingState.load` rebuilds the working view from the journal. When its in-memory cache is cold (a fresh `WorkingState`, which sync paths create), it replays the journal from offset 0. The journal is never truncated, since compaction is rdb-515fae. The old replay was eager:
- The initial base, every `checkpoint`, and every `commit` with a new base called `repo.SnapshotCommit`: an `ls-tree`, a manifest read and full validation.
- Every committed transaction was applied to the current snapshot.

Almost all of that was thrown away, because each checkpoint replaces the view. Replay cost was O(history), and a series of syncs quadratic.

## The fix (`common/repository/working.go`, `load`)

The key fact: **a checkpoint supersedes every transaction before it.** Only the last base, and the transactions committed after it, shape the final view.

- **Lazy base.** Records only move `view.baseCommit` and clear `baseLoaded`. `ensureBase()` loads the snapshot on demand and sets its generation, as the eager code did.
- **Deferred transactions.** A `commit` is still fully validated in order: matching prepare, monotonic generation, base-changed-while-dirty, and legacy manifest inventory. Prepare object checksums are verified when the prepare is read. Then the prepare is appended to `deferred` instead of being applied. A checkpoint or base change clears `deferred`.
- **`materialize()`** = `ensureBase()` + apply `deferred` in order. It runs before `reconcileHead` if the view is dirty (reconcile compares the dirty manifest with the head), and once at the end. If a clean view's base differs from the head, `reconcileHead` loads the head itself, and the stale base is marked as not needed.

A lazy base alone was insufficient. With "transaction, checkpoint, transaction…", every transaction forced its new base to load (still 21 loads for 20 checkpoints). Deferring the transactions is what makes it O(1).

`WorkingMetrics.SnapshotLoads` counts snapshot loads made by replay and reconciliation, through a small `snapshotCommit` helper, so tests can bound them.

## What changed semantically

Intermediate checkpoint snapshots and superseded transactions are no longer materialized during replay. Journal records are still validated in order, so corruption is detected as before. As a side benefit, an unreadable *old* checkpoint commit no longer breaks replay.

## Evidence

- `TestJournalReplayLoadsBoundedSnapshots`: 20 checkpoints, then a cold replay. The clean, dirty-tail and base-change cases each load exactly one snapshot, down from 21–22, with correct rows. The bound was tightened to ≤ 1 for all cases in review.
- Probe (journal, 1k rows, 10 syncs):
  - divergent merge 65.3 → 31.8 s total, with the last sync 9.6 → 2.9 s;
  - conflict resolve 80.0 → 44.0 s;
  - distinct commits listed per push flat at 3/4 (was 2 → 22); git calls per push flat at 58/84 (was 47 → 408).

## What's left

A flat ~3–4 s per journal merge sync at 1k rows, against native-git's ~1.3–1.5 s, mostly checkpoint plus git subprocess overhead (rdb-2c93ca). Compaction (rdb-515fae) would also stop replay from reading ever-growing frames, but frames are cheap next to snapshot loads.
