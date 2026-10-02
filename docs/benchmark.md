# Database scorecard: methodology

This document describes how RepoDB is benchmarked. It contains no results:
[benchmarks/latest.md](benchmarks/latest.md) is the one place that states how
RepoDB performs today. Earlier results are in that file's Git history
(`git log -p docs/benchmarks/latest.md`).

`dbbench` (`experiments/dbbench`) is the whole-database benchmark. It executes one
versioned workload suite and prints a consolidated table at the end, with raw
observations and metadata in a JSON report. Go benchmarks (`go test -bench`,
listed in [testing.md](testing.md#benchmarks)) are diagnostic tools, not the
scorecard.

## Setup

Published results compare like with like: **every system runs in a Linux
container on the same Docker VM.**

- **Where things run.** RepoDB (journal and native Git) runs embedded in the
  `dbbench` harness, inside a container built from `experiments/dbbench/Dockerfile`
  (Go toolchain image to build, `debian:trixie-slim` with Debian's `git` to run).
  MySQL and Dolt run in their own containers. For the external baselines, the
  harness runs in the same image and reaches them by container name over a
  user-defined Docker network (`repodb-bench`), so no request crosses the host's
  port forwarding.
- **Storage.** RepoDB's fixtures live on a Docker named volume
  (`repodb-bench-fixtures`); MySQL's and Dolt's data directories are named volumes
  too. All of them are ext4 inside the VM's disk image.
- **What a flush means there.** A synchronous write inside Docker Desktop's VM
  costs about 0.16 ms on the reference Mac. Guest flushes (`fdatasync`, InnoDB's
  log flush) don't become full flushes of the host SSD's cache, for any of the
  systems. Results therefore show what each system does on that VM, not how it
  would behave against power loss on bare hardware.
- **Durability settings.** Each system runs at its defaults: MySQL with
  `innodb_flush_log_at_trx_commit=1` and `sync_binlog=1`, Dolt as shipped, and
  RepoDB's journal at `normal` durability, which on Linux is an `fdatasync` per
  commit (see [architecture.md](architecture.md#durability-and-recovery)). Git's
  durability settings are never relaxed.
- **Resources.** No CPU or memory limits are set on any container. Containers run
  one at a time: the baselines are stopped while RepoDB runs, and only the
  baseline under test runs otherwise. Unrelated containers are stopped.
- **Recorded metadata.** Every report records `runtime` (`docker`), `platform`,
  Go and Git versions, `revision`, `working_tree_status`, `durability`, `git_gc`,
  and, for baselines, `server_version`.

`make bench-docker` builds the image, prepares the network and volume, and runs
one mode. It measures committed code only: with uncommitted changes it stops,
unless `BENCH_ALLOW_DIRTY=1` is set for a diagnostic run, whose report then
records the dirty status. Reports are written to `BENCH_OUT` (default
`./bench-out`).

```sh
# RepoDB, both modes
make bench-docker BENCH_MODE=native-git
make bench-docker BENCH_MODE=journal

# Baselines: attach their containers to the network once, then run each alone
docker network connect repodb-bench repodb-bench-mysql
docker network connect repodb-bench repodb-bench-dolt
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-mysql \
  BENCH_DSN='root@tcp(repodb-bench-mysql:3306)/'
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-dolt \
  BENCH_DSN='root@tcp(repodb-bench-dolt:3306)/'

# Quick harness check; these samples are not reference results
make bench-docker BENCH_ALLOW_DIRTY=1 BENCH_MODE=journal \
  BENCH_ARGS='-rows 100 -clients 1,4 -requests 2'
```

`make bench` and `make bench-external` run the same harness directly on the host.
They are for local profiling and diagnostics only and are never published: on
macOS, host runs use APFS and different flush primitives, so they don't compare
with the containerized baselines.

The defaults are 1k/10k/50k rows, 1/4/16 clients, and 30 requests per client for
each repeated workload. Git publication and sync make a complete run substantially
longer than a smoke test. The harness creates unique directories and retains
fixtures for inspection; `make bench-docker` empties the fixture volume before each
run. The MySQL-protocol workloads require permission to bind a loopback TCP port.

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
revision, working-tree status, Go/Git/platform versions, Git housekeeping policy (`git_gc`), and fixture path. Throughput
uses total workload wall time, including rejected attempts. Percentiles use
nearest-rank individual successful requests, not averages from separate runs.

Thirty observations do not establish stable p99 latency. Use larger request counts
and multiple fresh sequential invocations for reference measurements; keep all raw
reports. Use identical hardware, filesystem, power state, and flags, and record CPU,
mount/durability settings and other system load alongside the JSON. RepoDB's journal
commit durability is the harness's `-durability` flag (default `normal`, the
product default) and is recorded in the report as `durability`; compare runs
only at the same level. Git's durability settings are never relaxed. Filesystem caches are uncontrolled; fresh
process reopen is not a cold-filesystem or power-loss test.

**Git housekeeping.** Left to its defaults, Git starts `maintenance run --auto`
(and `gc --auto`) detached after fetches and pushes, so a repack can overlap
whichever timed request runs next. Every fixture repository therefore sets
`gc.auto=0`, `maintenance.auto=false` and `receive.autogc=false`, and the harness
runs `git gc --quiet` untimed in the local repository and its remote after the
initial publication and before the concurrency, MySQL-interface and sharing
groups. The report records this as `"git_gc": "manual"`. Housekeeping cost is not
part of any measured workload.

Native-git saves write and fsync Git objects on every commit. Occasional
write stalls inside those Git processes, unrelated to housekeeping, can still
appear in native-git p95/max (rdb-c85855).

Allocation and heap figures are process-wide, including harness/client overhead.
Peak RSS spans the whole run and excludes child processes. File growth sums logical
file lengths across the local repository, remote, and peer; it is neither physical
disk allocation nor network transfer volume. Setup/client connection establishment
is outside repeated SQL request timing. There is no artificial warmup.

## External baselines

`-mode external` runs the portable SQL workloads against an external
MySQL-compatible server. Git-specific operations (sync, merge, conflict,
reopen) are skipped; everything else — reads, writes, concurrency, correctness
checks — uses the same workload code and reporting format.

A fresh database is created per fixture size and dropped on success. The DSN
follows `go-sql-driver/mysql` format (`user:pass@tcp(host:port)/`). Results are
directly comparable to the native-git and journal runs in the setup above:
identical SQL, identical correctness checks, identical reporting. Commands are
under [Setup](#setup).

## Explicit coverage limits

This is one extensible scorecard, not proof of every database property. Version 1
does not measure power-loss recovery, network latency/bandwidth, process-level
writer contention, live SQL latency during checkpoint/compaction, realistic board
requests, or independent history-depth scaling. Crash/fault correctness remains
in the existing test suite.
Add future dimensions here and to the same runner, rather than introducing another
headline benchmark. Change the suite version when workload
semantics change, and never compare different suite versions as matched results.

## Publishing results

[benchmarks/latest.md](benchmarks/latest.md) is the only place that states current
numbers. The README performance table quotes it, and nothing else does.

To publish a new scorecard:

1. **Measure at one clean commit.** Run every mode (native-git, journal, and the
   MySQL and Dolt baselines) with `make bench-docker`, sequentially on the same
   machine and power state, from the same commit with no local changes (see
   [Setup](#setup)). Each JSON report records
   `revision` and `working_tree_status`: all revisions must match, and the status must
   be empty.
2. **Replace latest.md and `docs/benchmarks/latest/*.json` in place.** The previous
   version stays in Git history; do not copy it elsewhere in `docs/`.
3. **Write the new latest.md** from the new runs. State the revision, date, machine,
   OS, filesystem, Go, Git, MySQL and Dolt versions, and the exact commands. Link the
   raw JSON in `docs/benchmarks/latest/`. Use the table order in latest.md: single-client
   SQL latency, then concurrency, then sync.
4. **Compare with the previous run** (`git show <previous-commit>:docs/benchmarks/latest/<file>.json`)
   in a "Changes since `<previous revision>`" section. Use the unchanged MySQL and Dolt
   runs as the noise reference, and claim only changes well beyond it.
5. **Update the README table** from latest.md, including its measurement date.

Diagnostic measurements, such as a Go benchmark study for one change, belong in
that change's xpo issue (comment or walkthrough), not in `docs/`.
