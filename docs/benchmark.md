# Database scorecard

`make bench` is the current whole-database benchmark entry point. It executes one
versioned workload suite against the current checkout and prints a consolidated
table at the end, with raw observations and metadata in `dbbench.json`.
Historical milestone reports and Go microbenchmarks remain diagnostic tools;
they are not the current database scorecard.

```sh
make bench
make bench BENCH_MODE=journal

# Quick harness verification; these samples are not performance reference results.
make bench BENCH_MODE=journal \
  BENCH_ARGS='-rows 100 -clients 1,4 -requests 2 -output /tmp/journal-smoke.json'

# Choose the filesystem and preserve results from separate, sequential runs.
make bench BENCH_MODE=native-git \
  BENCH_ARGS='-temp-dir /Volumes/Work/Projects/repodb -output /tmp/git-scorecard.json'
make bench BENCH_MODE=journal \
  BENCH_ARGS='-temp-dir /Volumes/Work/Projects/repodb -output /tmp/journal-scorecard.json'
```

The defaults are 1k/10k/50k rows, 1/4/16 clients, and 30 requests per client for
each repeated workload. Git publication and sync make a complete run substantially
longer than a smoke test. The harness creates unique directories, never resets
an existing repository, and retains fixtures for inspection. Remove the printed
fixture directory when finished. The MySQL tests require permission to bind a
loopback TCP port.

## Workload contract (version 1)

Both modes run exactly the same workload code. Persistence selection is confined
to database construction and the adapter that makes local changes available for
sync. That adapter checkpoints when required before invoking sync. Its complete
cost is inside every measured sync workflow. Ordinary SQL saves do not checkpoint.
Native Git therefore pays publication during saves; journal pays it during sync.
Do not compare save latency alone as the cost of the complete workflow.

Every size starts with a fresh repository and deterministic integer keys and
32-byte text values. Workloads execute in a fixed order against that evolving
fixture; history and journal growth are intentional. Sharing scenarios each use
an independent pair of repositories seeded from the same committed data so a
failed merge cannot contaminate the conflict-resolution scenario. Initial data is published
before reads. First-save transitions remain in the raw observations.

| Dimension | Measured operations |
| --- | --- |
| Lifecycle and ingestion | Open, schema creation, bulk load including commit, initial publication/sync |
| SQL reads | Autocommit point hit/miss, 100-row range, descending top 20, full scan, join/aggregate, transaction containing ten point reads |
| SQL writes | Explicit transactions of 1/10/100 exact-key updates, autocommit inserts/deletes, rollback |
| Concurrency | One session per client; every fifth mixed-workload request writes, remaining requests read; separate contended counter increments |
| MySQL interface | Persistent loopback client point reads and autocommit updates, including network and result consumption |
| Reopen | New process opens existing durable state and checks rollback marker, row count, and latest wire write; includes process startup |
| Sharing | Publication after accumulated writes, initialize/adopt/open peer, unchanged sync, edit/sync round trip, divergent edits/merge, conflict detection/resolution/convergence |
| Resources | File-size growth per workload, Go allocation totals and end heap, whole-process peak RSS |

The join fixture has one author; it is a SQL execution check, not the proposed
realistic issue/author/comment/event application dataset. A request in a batch
workload is an entire transaction; a request in a sharing workload includes all
listed saves, synchronization, and convergence checks. Bulk load and setup occur
once per size. Reopen runs at most five times per size. Other workloads repeat
according to `-requests`.

Correctness checks cover result cardinalities, final update values, insert/delete
counts, rollback visibility, recovered data, remote convergence, conflict choices,
and a counter equaling the number of acknowledged concurrent increments. Expected
publication conflicts are counted separately, followed by rollback before reusing
the session. There are no retries. Other errors fail the run with a nonzero exit
status and retain a partial report. Failed counter verification marks the workload
FAIL even when SQL returned success. Independent sharing scenarios and subsequent
fixture sizes continue after failures; dependent work stops if setup fails.

## Reading results

The table reports successful operations/sec, successful request p50/p95/p99/max,
success/conflict/error counts, and fixture growth. JSON additionally retains raw
successful and rejected request durations, process allocations, heap, configuration,
revision, working-tree status, Go/Git/platform versions, and fixture path. Throughput
uses total workload wall time, including rejected attempts. Percentiles use
nearest-rank individual successful requests, not averages from separate runs.

Thirty observations do not establish stable p99 latency. Use larger request counts
and multiple fresh sequential invocations for reference measurements; keep all raw
reports. Use identical hardware, filesystem, power state, and flags, and record CPU,
mount/durability settings and other system load alongside the JSON. No durability
settings are relaxed by this harness. Filesystem caches are uncontrolled; fresh
process reopen is not a cold-filesystem or power-loss test.

Allocation and heap figures are process-wide, including harness/client overhead.
Peak RSS spans the whole run and excludes child processes. File growth sums logical
file lengths across the local repository, remote, and peer; it is neither physical
disk allocation nor network transfer volume. Setup/client connection establishment
is outside repeated SQL request timing. There is no artificial warmup.

## Explicit coverage limits

This is one extensible scorecard, not proof of every database property. Version 1
does not measure power-loss recovery, network latency/bandwidth, process-level
writer contention, live SQL latency during checkpoint/compaction, realistic board
requests, independent history-depth scaling, secondary indexes, or external
MySQL/Dolt baselines. Crash/fault correctness remains in the existing test suite.
Add future dimensions here and to the same runner, rather than introducing another
milestone-specific headline benchmark. Change the suite version when workload
semantics change, and never compare different suite versions as matched results.

## Initial harness verification

The 100-row, 1/4-client, two-request smoke runs on the 2026-09-13 working checkout
exposed failures beyond the existing passing test suite: journal mode rejects a
second divergent merge or conflict-resolution cycle with `transaction base changed`,
and native-Git contended increments have produced a final counter below the number
of acknowledged increments. These are observed failures requiring engine investigation,
not benchmark exclusions or performance reference results. The runner preserves
the failed results and exits nonzero. No engine behavior was changed to make this
scorecard pass.
