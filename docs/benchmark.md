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

## Reference comparison (2026-09-13)

Apple M1 Max, macOS 15.7, APFS, Go 1.27.1, Git 2.55.0. RepoDB runs embedded;
MySQL 8 and Dolt run in Docker containers on the same machine. All runs use the
same workload code, 30 requests per workload, correctness checks passing. MySQL
and Dolt skip Git-specific operations (sync, merge, conflict, reopen). Raw JSON
is in `docs/scorecard-{native-git,journal,mysql,dolt}.json`.

### Single-client SQL latency, p50 ms

| Workload | Rows | MySQL 8 | Dolt | RepoDB Journal | RepoDB Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Point read | 1k | 0.2 | 0.3 | 6.8 | 27 |
| Point read | 10k | 0.2 | 0.3 | 6.6 | 38 |
| Point read | 50k | 0.2 | 0.4 | 7.0 | 52 |
| Range (100) | 1k | 0.4 | 0.5 | 8.5 | 29 |
| Range (100) | 10k | 0.4 | 0.5 | 25 | 56 |
| Range (100) | 50k | 0.4 | 0.5 | 95 | 138 |
| Full scan | 1k | 15 | 15 | 8.6 | 29 |
| Full scan | 10k | 16 | 19 | 25 | 55 |
| Full scan | 50k | 29 | 38 | 96 | 140 |
| Update x1 | 1k | 1.3 | 1.6 | 6.1 | 192 |
| Update x1 | 10k | 1.4 | 1.7 | 12 | 278 |
| Update x1 | 50k | 1.3 | 1.6 | 13 | 414 |
| Update x10 | 50k | 5.5 | 7.2 | 15 | 356 |
| Update x100 | 50k | 59 | 80 | 29 | 369 |
| Insert | 50k | 1.2 | 1.0 | 6.0 | 209 |
| Delete | 50k | 1.2 | 1.0 | 6.1 | 209 |
| Rollback | 50k | 1.1 | 0.8 | 0.1 | 105 |

### Concurrency, 50k rows, p50 ms

| Workload | Clients | MySQL 8 | Dolt | RepoDB Journal | RepoDB Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mixed 80/20 read/write | 4 | 0.7 | 0.5 | 1.2 | 70 |
| Mixed 80/20 read/write | 16 | 1.6 | 0.7 | 2.2 | 87 |
| Contended increment | 4 | 2.0 | 1.7 | 34 | 261 |
| Contended increment | 16 | 8.5 | — | 61 | 295 |

Dolt's 16-client contended increment fails with a serialization error (MySQL
error 1213); the harness does not map it to a retryable conflict.

### Sync latency, 50k rows, p50 ms (RepoDB only)

| Workload | Native Git | Journal |
| --- | ---: | ---: |
| Sync unchanged | 158 | 236 |
| Edit/sync roundtrip | 1,023 | 6,394 |
| Divergent merge | 2,550 | 18,266 |
| Conflict resolve | 2,812 | 23,887 |

Journal-mode sync is 6–8x slower because each sync iteration checkpoints
accumulated journal state to Git before exchanging history. Thirty iterations
compound checkpoint cost with growing repository and journal size.

### What the numbers say

**Reads.** MySQL and Dolt serve point reads in sub-millisecond time from
in-memory indexes. RepoDB journal takes 7 ms because every transaction start
loads table metadata from content-addressed Prolly objects; native-git adds
Git-ref resolution on top. Range and scan latency grows with row count in
RepoDB because it materializes rows from the Prolly tree; MySQL and Dolt
keep pages in a buffer pool.

**Small writes.** MySQL and Dolt commit a single-row update in 1–2 ms.
RepoDB journal takes 5–13 ms (Prolly mutation plus one `fsync`). Native-git
takes 130–414 ms (full Git snapshot: object hashing, tree construction,
commit, ref CAS).

**Batch writes.** RepoDB journal (29 ms for 100 updates) beats Dolt (80 ms)
and approaches MySQL (59 ms) because the journal appends one framed record
regardless of batch size. Native-git stays at 370 ms because Git publication
cost is per-transaction, not per-row.

**Concurrency.** All four handle mixed read/write well. Contended single-row
increments expose serialization overhead: MySQL uses row locks (8.5 ms at 16
clients), RepoDB journal uses generation-based CAS (61 ms), native-git uses
Git ref CAS (295 ms with 80% conflict rate).

**Trade-off.** Neither MySQL nor Dolt provides Git-native version history,
cross-clone synchronization, or merge. RepoDB's read and write overhead is
the cost of content-addressed storage and deterministic trees that make those
features possible. The journal prototype reduces write latency to within 5–10x
of MySQL for small operations; reads remain the larger gap and the next
optimization target.

## Explicit coverage limits

This is one extensible scorecard, not proof of every database property. Version 1
does not measure power-loss recovery, network latency/bandwidth, process-level
writer contention, live SQL latency during checkpoint/compaction, realistic board
requests, or independent history-depth scaling. Crash/fault correctness remains
in the existing test suite.
Add future dimensions here and to the same runner, rather than introducing another
milestone-specific headline benchmark. Change the suite version when workload
semantics change, and never compare different suite versions as matched results.

## Initial harness verification

The 100-row, 1/4-client, two-request smoke runs on the initial 2026-09-13 working
checkout exposed two failures beyond the existing passing test suite:

1. **Journal divergent merge** (`transaction base changed`): after sync advances
   the Git head, journal replay rejected the next transaction because its base
   commit differed from the checkpoint's. Fixed by allowing a clean journal's base
   to advance during replay when an external operation (sync) changed the Git head
   between a checkpoint and the subsequent transaction.

2. **Native-Git contended increment** (counter below acknowledged increments):
   when two concurrent writers produced identical Git commits (same tree, parents,
   and second-resolution timestamp), `resolvePublication` falsely reported the
   second writer's CAS failure as committed because the ref pointed at the same
   hash. Fixed by treating any ref advancement past the expected head as a conflict,
   regardless of whether the current ref equals the candidate commit.

Both fixes preserve the existing test suite, race tests, and the documented
outcome-recovery contract. The smoke runs now pass in both modes.
