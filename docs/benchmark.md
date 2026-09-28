# Database scorecard: methodology

This document describes how RepoDB is benchmarked. It deliberately contains no
results:

- **Current results:** [benchmarks/latest.md](benchmarks/latest.md), the one
  canonical source for how RepoDB performs today.
- **Past results:** [benchmarks/history/](benchmarks/history/), dated reports that
  describe the repository as it was when they were measured.

`make bench` is the whole-database benchmark entry point. It executes one
versioned workload suite against the current checkout and prints a consolidated
table at the end, with raw observations and metadata in `dbbench.json`. It
defaults to native-Git persistence (`BENCH_MODE=native-git`). Go microbenchmarks
(`make m4.2-bench`, `make m4.3-bench`, `go test -bench`) are diagnostic tools,
not the scorecard.

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

## External baselines

`make bench-external` runs the portable SQL workloads against an external
MySQL-compatible server. Git-specific operations (sync, merge, conflict,
reopen) are skipped; everything else — reads, writes, concurrency, correctness
checks — uses the same workload code and reporting format.

```sh
# MySQL (default DSN)
make bench-external

# Dolt (default sql-server port)
make bench-external BENCH_DSN='root:@tcp(127.0.0.1:3336)/'

# Quick smoke
make bench-external BENCH_DSN='root@tcp(127.0.0.1:3306)/' \
  BENCH_ARGS='-rows 100 -clients 1,4 -requests 2 -output /tmp/mysql-smoke.json'
```

A fresh database is created per fixture size and dropped on success. The DSN
follows `go-sql-driver/mysql` format (`user:pass@tcp(host:port)/`). Results are
directly comparable to the native-git and journal runs on the same machine:
identical SQL, identical correctness checks, identical reporting. Compare all
four columns (native-git, journal, MySQL, Dolt) from sequential runs on the
same hardware, filesystem, and power state.

## Explicit coverage limits

This is one extensible scorecard, not proof of every database property. Version 1
does not measure power-loss recovery, network latency/bandwidth, process-level
writer contention, live SQL latency during checkpoint/compaction, realistic board
requests, or independent history-depth scaling. Crash/fault correctness remains
in the existing test suite.
Add future dimensions here and to the same runner, rather than introducing another
milestone-specific headline benchmark. Change the suite version when workload
semantics change, and never compare different suite versions as matched results.

## Publishing results

[benchmarks/latest.md](benchmarks/latest.md) is the only place that states current
numbers. The README performance table quotes it, and nothing else does.

To publish a new scorecard:

1. **Measure at one clean commit.** Run every mode (native-git, journal, and the
   MySQL and Dolt baselines) sequentially on the same machine, filesystem, and power
   state, from the same commit with no local changes. Each JSON report records
   `revision` and `working_tree_status`: all revisions must match, and the status must
   be empty.
2. **Archive the previous latest.** `git mv docs/benchmarks/latest.md
   docs/benchmarks/history/<date>-<short-rev>-scorecard.md` and move its JSON
   directory alongside it (`docs/benchmarks/history/<date>-<short-rev>-scorecard/`).
   Add the history banner (below) and fix relative links. Change nothing else in
   the archived report.
3. **Write the new latest.md** from the new runs. State the revision, date, machine,
   OS, filesystem, Go, Git, MySQL and Dolt versions, and the exact commands. Link the
   raw JSON in `docs/benchmarks/latest/`. Use the table order in latest.md: single-client
   SQL latency, then concurrency, then sync.
4. **Update the README table** from latest.md, including its measurement date.

Milestone or diagnostic reports (for example a Go microbenchmark study) go straight
into `docs/benchmarks/history/<date>-<topic>.md` with the banner. They never become
latest.md.

Every history file starts with this banner, filled in:

```markdown
> **Historical record.** <What was measured>, measured <date> at <revision(s)> on
> <machine>. It describes the repository at that time and is not maintained.
> Current results are in [latest.md](../latest.md).
```
