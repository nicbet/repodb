# rdb-162912 walkthrough: stale writers fail fast under the publication lock

## How it surfaced

The scorecard run at `f944eee` (rdb-13fbd8) showed native-git `contended_increment` at 16 clients dropping from 4 to 1 ops/s, with rejections up from 68 % to 94 %. A probe bisected across `cfa8eed` → `4b66f4a` → `f944eee` put the regression on rdb-e0c717. The first probe ran autocommit without rolling back after conflicts, which tripped rdb-ed0738 on the old commit; the bisect only worked once it rolled back like dbbench.

## Why rdb-e0c717 caused it

rdb-e0c717 routed native-git publications through `lockForPublication`: the guard `WorkingState`'s in-process mutex, then `working.lock`, then `publish.lock`. That queues writers roughly FIFO, where before they polled one file lock every 10 ms. Under contention, every queued writer whose base had gone stale still ran the whole publication under the lock: `hash-object`, `read-tree`, `update-index`, `write-tree`, `commit-tree`. Only then did `update-ref`'s compare-and-swap reject it. Fifteen doomed publications per round, serialized.

Under the old polling, a client that had just rolled back could barge in with a fresh snapshot and win. That gave ~50 % rejections and higher throughput, by accident and unfairly.

## The fix

In `Writer.CommitWithOutcomeMessage`, right after the locks are held: if the writer has an expected head and `Repository.Head` reports a different one, return `ErrConflict` with outcome `rejected`, before writing anything. Holding the locks means the head can't change underneath us, so this rejects exactly the writers the CAS would reject, only sooner. `Head` is cheap here: its ref-stamp cache makes an unchanged ref a `stat`. The CAS remains the authority, and a failed head read falls through to it.

## Results

Probe at 16 clients: 1.4 → ~9.6 ops/s and p50 732 → ~100 ms, above the pre-regression 7.3 ops/s, since Prolly commits are also cheaper after rdb-fb3d12. Rejections stay at the FIFO lock-step rate (one winner per round), which is the honest optimistic-concurrency answer. Reducing them is rdb-df092b (per-key conflicts, retry).

## Test

`TestStaleWriterIsRejectedBeforeWritingObjects` asserts `git count-objects -v` is unchanged after a stale commit. Before the fix, 13 loose objects were written.
