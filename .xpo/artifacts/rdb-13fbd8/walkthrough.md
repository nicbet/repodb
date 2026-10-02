# Walkthrough: scorecard refresh at 87dd885

## What was published

- `docs/benchmarks/latest.md` and `docs/benchmarks/latest/*.json`: the four-way scorecard (suite v1, default config) measured at `87dd885` on a clean tree, replaced in place per `docs/benchmark.md`.
- The README performance table, generated from the JSON with the same formatter as latest.md, with the new date and revision and journal described as the default mode.

## How it got there

1. **First run at `f944eee`.** All four modes completed cleanly, but native-git `contended_increment` had regressed (16 clients: 4 → 1 ops/s). A probe bisected it to rdb-e0c717: FIFO publication queueing made stale writers do their full Git publication under the lock. With the user's agreement, it was fixed first in rdb-162912 (`87dd885`). The `f944eee` JSONs stayed in the scratchpad as diagnostics.
2. **Final run at `87dd885`.** Same order, isolation and binaries as before. Running containers were logged at each phase. The `servaint-*` containers had been restarted between the runs and were stopped again with the user's OK.
3. **The journal initial publish slowdown** (7.4 → 9.0 s) was bisected with a 50k-row bulk-checkpoint probe. Compaction (rdb-4d677f) was ruled out; the cause is rdb-fb3d12's boundary rule producing ~17 % smaller chunks (65 vs 78 rows per node) for BIGINT keys, so a bulk publish writes ~20 % more objects. Filed as rdb-a0d510 and stated in latest.md.

## How latest.md was written

- **Tables.** A render script, validated by reproducing the `c1aa399` Results section exactly, generates every table. Its number formatting is two decimals below 1, one below 10, then whole numbers with separators; values that round into the next band take that band's format.
- **Noise.** Two runs on near-identical code (`f944eee`, `87dd885`) showed ~25 % variation on single-client Git-heavy RepoDB workloads, and MySQL and Dolt p50s moved up to 1.5×. "Changes since `c1aa399`" lists only changes beyond that, each tied to a mechanism:
  - native-git write tails (rdb-fb3d12);
  - native-git mixed throughput (rdb-162912, rdb-fb3d12);
  - contended increment, including why rejections rose to the FIFO lock-step rate;
  - publish + sync after writes and journal sync tails (rdb-2124bc, rdb-fb3d12);
  - the bulk publish regression (rdb-a0d510).
- **Not claimed:** native-git insert and delete p50 (+10 %, within the two-run spread), read-transaction p50 (−28 % in both modes, within the baseline band and with no mechanism), and sync p50 wobble.
- **Accuracy fix.** Dolt's 1k-row fixture stopped at its contended-increment error, so "no fixture stopped" was replaced with what actually happened.

## Follow-ups

- rdb-a0d510: tune the Prolly chunk size (BACKLOG).
- rdb-df092b: per-key conflicts to reduce the lock-step rejection rate.
