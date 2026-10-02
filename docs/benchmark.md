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
  BENCH_ARGS='-rows 100 -workload-rows 1000 -growth-rounds 2 -clients 1,4 -requests 2'

# One workload group only, for a diagnostic run
make bench-docker BENCH_ALLOW_DIRTY=1 BENCH_MODE=journal \
  BENCH_ARGS='-workloads mutable'
```

`make bench` and `make bench-external` run the same harness directly on the host.
They are for local profiling and diagnostics only and are never published: on
macOS, host runs use APFS and different flush primitives, so they don't compare
with the containerized baselines.

Two flags support diagnosis and are never used for published runs.
`-cpuprofile <file>` writes a CPU profile of the whole run. Every sample taken in
a timed request or growth phase carries a `workload` label (and `clients` for
measured workloads), so `go tool pprof -tagfocus workload=issue_mixed_concurrent`
isolates one workload. `-pprof <addr>` serves `net/http/pprof` with mutex and
block profiling enabled, for heap or contention captures while a run is in
progress. Profile inside the Linux container: Go CPU profiles taken on macOS
undercount.

A run executes three workload groups, selected with `-workloads` (default
`core,append,mutable`; see [Workload contract](#workload-contract-version-2)). The
defaults are 1k/10k/50k rows for `core` (`-rows`), 1k/10k/100k rows for `append`
and `mutable` (`-workload-rows`), 10 growth rounds (`-growth-rounds`), 1/4/16
clients, and 30 requests per client for each repeated workload. Git publication and sync make a complete run substantially
longer than a smoke test. The harness creates unique directories and retains
fixtures for inspection; `make bench-docker` empties the fixture volume before each
run. The MySQL-protocol workloads require permission to bind a loopback TCP port.

## Workload contract (version 2)

Version 2 has three groups. `core` is the whole of version 1, unchanged: its rows
compare with a version 1 report's. `append` and `mutable` are application-shaped
and new in version 2. The JSON report records `workloads`, and every `append` or
`mutable` measurement carries a `group` field (`core` rows have none).

### Core

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

The join fixture has one author; it is a SQL execution check; the application-shaped
datasets are the `append` and `mutable` groups. A request in a batch
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

### Append and mutable

These two groups model the two shapes RepoDB's consumers run. Each size starts
from a fresh repository (or, in external mode, a fresh database). Fixtures are
deterministic: every row is a fixed function of its id, and text is
pseudo-prose drawn from a fixed vocabulary, so it compresses like real text,
not like random bytes. Unlike `core`, both groups use one workload
implementation for RepoDB and external servers, so an external run executes
the identical SQL.

**`append`: an agent trace or event log.** `events (id BIGINT PRIMARY KEY, ts,
session_id, kind, payload TEXT)` has secondary indexes `(session_id, ts)` and
`(ts)`. Ids and timestamps increase together, so appends land at the right edge
of every tree. Sessions are contiguous runs of 100 events, there are 8 kinds,
and payloads are 256-byte JSON. The size tier is the number of preloaded
events.

| Workload | Request |
| --- | --- |
| `events_recent` | newest 100 events in the last 1,000 by `ts` (checks the newest id) |
| `events_session` | one session's 100 events in `ts` order |
| `events_kind_count` | `GROUP BY kind` over the newest 10% (checks all 8 kinds) |
| `append_single` | autocommit insert of one event |
| `append_batch_100` | transaction of 100 inserts |
| `event_update_rare` | autocommit payload rewrite of a uniformly random old event |
| `append_concurrent` | one session per client, one autocommit insert per request, ids from a shared counter |

**`mutable`: an issue board.** `issues (id BIGINT PRIMARY KEY, status, assignee,
priority, title, body TEXT, updated_at)` has secondary indexes
`(status, priority)` and `(assignee, status)`. `comments (id, issue_id, body)`
has a secondary index on `(issue_id)`. Bodies are 1 KiB (issues) and 200 bytes
(comments). There are 5 statuses and 20 assignees, and about one issue in eleven
is unassigned. The size tier is the number of issues, with two comments per
issue preloaded. Write targets are hot rows: issue ids are Zipf-distributed
(s = 1.1) toward the newest issues. The newest fifth receives 84% of draws at
1k issues and 93% at 100k, and at 100k three quarters of draws fall on the newest
1%.

| Workload | Request |
| --- | --- |
| `issue_point_read` | one hot issue by id |
| `issue_board_query` | `WHERE status = ? ORDER BY priority LIMIT 50` |
| `issue_assignee_query` | one assignee's issues that are not done |
| `issue_update_status` | autocommit status and `updated_at` change on a hot issue |
| `issue_update_edit` | transaction rewriting a hot issue's title and body |
| `issue_comment` | transaction inserting a comment and bumping its issue's `updated_at` |
| `issue_mixed_concurrent` | one session per client: 20% status updates, 10% comments, 70% point, board and assignee reads |

Each group first records `<group>_open`, `<group>_schema`, `<group>_bulk_load`
(one transaction of 500-row statements, as in `core`) and
`<group>_initial_publish_sync`. It then runs the reads above against the
published preload, then the writes. Concurrent workloads count rejected writes as
conflicts, as `core` does. External mode reports MySQL error 1213 (a deadlock,
or a Dolt serialization failure) and 1205 (lock wait timeout) as conflicts.

**Growth phase.** Each group ends with `-growth-rounds` rounds. A round runs a
fixed write burst, then syncs, without Git housekeeping in between, so the series
shows unmaintained growth:

- `append`: 1,000 events in ten 100-event transactions, each followed by one rare
  update.
- `mutable`: 500 hot status updates and 100 comments.

Every round adds a point to the report's `series`, with the table's row count,
the burst's write p50/p95/max, the sync time, absolute fixture bytes (local
repository plus remote) and their change, journal bytes before and after the
sync (journal mode), and the process heap. The printed table shows the first,
middle and last rounds. A peer is then cloned and opened once
(`<group>_peer_pull_after_growth`, the whole accumulated history), and its
checksum queries must match the local database's. External mode runs the bursts
but has no sync, sizes or peer.

**Correctness checks.** Final event and comment counts, the newest event id, the
last edit of the most-edited hot issue, and peer convergence. A failure marks the
workload FAIL under the same rules as `core`.

## Reading results

The table reports successful operations/sec, successful request p50/p95/p99/max,
success/conflict/error counts, and fixture growth, in one section per workload
group, followed by the growth series. JSON additionally retains raw
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

`-mode external` runs the portable SQL workloads, including the `append` and
`mutable` groups, against an external MySQL-compatible server. Git-specific
operations (sync, merge, conflict, reopen, peers, and the growth phase's syncs)
are skipped; everything else — reads, writes, concurrency, correctness
checks — uses the same workload code and reporting format.

A fresh database is created per fixture size and dropped on success. The DSN
follows `go-sql-driver/mysql` format (`user:pass@tcp(host:port)/`). Results are
directly comparable to the native-git and journal runs in the setup above:
identical SQL, identical correctness checks, identical reporting. Commands are
under [Setup](#setup).

## Explicit coverage limits

This is one extensible scorecard, not proof of every database property. Version 2
does not measure power-loss recovery, network latency/bandwidth, process-level
writer contention, live SQL latency during checkpoint/compaction, or deep history:
the growth phase adds at most ten rounds of writes per size. Crash/fault correctness remains
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
