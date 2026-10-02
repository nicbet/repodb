# Latest benchmark results

This file is the one canonical source for RepoDB's current performance. The
methodology, the setup and the process for replacing this file are in
[benchmark.md](../benchmark.md). Earlier results are in this file's Git history
(`git log -p docs/benchmarks/latest.md`).

**Measured 2026-10-02 at `636af3d`** (clean tree), scorecard suite v1 with default
configuration: 1k/10k/50k rows, 1/4/16 clients, 30 requests per client per
repeated workload. Raw reports: [native-git](latest/scorecard-native-git.json),
[journal](latest/scorecard-journal.json), [MySQL](latest/scorecard-mysql.json),
[Dolt](latest/scorecard-dolt.json).

**Every system ran in a Linux container on the same Docker VM**, with its data on
Docker named volumes, at its default durability settings: like for like. See
[benchmark.md § Setup](../benchmark.md#setup) for what that means, including what a
flush costs there. Journal is RepoDB's default persistence mode; native Git is the
opt-in audit mode, in which every transaction is a Git data commit.

## Environment

| | |
| --- | --- |
| Host | Apple M1 Max, 64 GB, on AC power, macOS 15.8 |
| Docker VM | Docker Desktop 29.8.1, kernel 7.0.14-linuxkit, 4 CPUs, 16 GB; data on named volumes (ext4 in the VM's disk image) |
| RepoDB harness | image built from `experiments/dbbench/Dockerfile`: Go 1.27.1, Debian trixie, Git 2.47.3; RepoDB built with `-tags gms_pure_go`, embedded in the harness |
| Baselines | MySQL 8.4.11 (`mysql:8.4`, `sha256:0744ee5e…`) and Dolt 2.3.5 (`dolthub/dolt-sql-server:latest`, `sha256:36fdd43d…`), reached by container name over the `repodb-bench` network |
| Durability | MySQL `innodb_flush_log_at_trx_commit=1`, `sync_binlog=1`; Dolt as shipped; RepoDB journal `normal` (an `fdatasync` per commit on Linux) |
| Isolation | Sequential runs, one container at a time (the harness plus at most the baseline under test); unrelated containers stopped. Ordinary desktop applications stayed open. |
| Git housekeeping | Fixture repositories disable automatic gc and maintenance; `git gc` runs untimed between workload groups (`"git_gc": "manual"`). |

## Commands

From a clean checkout of `636af3d`, with the baseline containers attached to the
`repodb-bench` network and started one at a time:

```sh
make bench-docker BENCH_MODE=native-git
make bench-docker BENCH_MODE=journal
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-mysql BENCH_DSN='root@tcp(repodb-bench-mysql:3306)/'
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-dolt  BENCH_DSN='root@tcp(repodb-bench-dolt:3306)/'
```

Wall time: native-git 2.4 min, journal 2.5 min, MySQL 5 s, Dolt 7 s.

## Summary

All figures are p50; ranges span 1k–50k rows unless a size is named.

- **Single-row writes (journal)** take 0.31–0.56 ms: about MySQL's 0.51–0.72 ms
  and faster than Dolt's 0.71–1.0 ms. Batches amortize: 100 updates take
  3.0–3.3 ms, against 8.4–9.1 ms for MySQL and 25 ms for Dolt.
- **Point reads** take 0.06–0.08 ms in RepoDB, embedded in the harness; MySQL takes
  0.03–0.04 ms and Dolt 0.10–0.14 ms, each including a TCP round trip between
  containers. Through RepoDB's own MySQL-protocol server, measured the same way, a
  journal point read at 50k rows takes 0.06 ms (`mysql_point_read` in the raw JSON).
- **Range reads** (100 rows) take 0.11–0.18 ms (MySQL 0.08–0.15 ms, Dolt 0.26 ms),
  and a transaction of ten point reads 0.52–0.71 ms (MySQL 0.65–0.94 ms, Dolt
  1.9–2.1 ms).
- **Full scans and joins** are where RepoDB trails most: at 50k rows a full scan
  takes 34 ms (MySQL 13 ms, Dolt 20 ms) and the join, which reads every `bench` row,
  43–45 ms (MySQL 2.6 ms, Dolt 12 ms). The descending **ordered limit** (`ORDER BY id
  DESC LIMIT 20`) sorts the whole table, 39 ms at 50k rows, until reverse index scans
  land (rdb-acd36d); the ascending form is served from the index.
- **Native-Git writes** publish a Git snapshot per transaction: 9.5–19 ms at 1k rows,
  growing to 39–51 ms at 50k rows.
- **Contention** is handled by rejecting writers, not queueing them: a write
  transaction is rejected when any other transaction committed after its snapshot,
  even one that wrote different rows. In the mixed workload, where each client
  updates its own row, 2–5% (journal) and 15–19% (native Git) of attempts are
  rejected at 50k rows; under a contended increment, 7–13% and 75–93%. Successful
  write throughput is far below MySQL's.
- **Sync** at 50k rows: divergent merge takes 0.66 s (native Git) and 1.1 s
  (journal), conflict resolution 0.82 s and 1.3 s; the journal's initial publish
  and sync of the bulk-loaded table takes 0.94 s.
- **Dolt 2.3.5 failed correctness** under concurrent increments (lost acknowledged
  updates, serialization errors). Its contended-increment numbers are marked ✗ and
  are not comparable. MySQL, journal and native Git passed every check.

## Changes since `87dd885`

The setup changed, so this run does not compare number for number with the
previous one (this file as of `859d107`):

- **RepoDB now runs in a container** (rdb-d58a06). It previously ran on the macOS
  host, where every journal commit was a full flush of the SSD's cache
  (`F_FULLFSYNC`, ~5 ms) and native-Git commits paid one per Git object, while the
  baselines' flushes inside the VM never reached the host SSD as full flushes.
- **The baselines are measured from inside the VM** (rdb-d58a06). The harness
  previously reached them from the host through Docker Desktop's port forwarding,
  which added roughly 0.15–0.2 ms to every MySQL and Dolt request. Their read
  latencies are lower now for that reason alone.
- **Journal commits default to `normal` durability** (rdb-d690aa); on Linux that is
  an `fdatasync` per commit, the same flush as `full`.

Both earlier distortions favored one side: host-side reads made RepoDB's reads look
faster than MySQL's, and host-side flushes made its writes look slower.

## Results

### Single-client SQL latency, 1k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.03 | 0.10 | 0.07 | 0.06 |
| Point miss | 0.03 | 0.10 | 0.06 | 0.05 |
| Range (100) | 0.08 | 0.26 | 0.15 | 0.18 |
| Ordered limit | 0.04 | 0.15 | 0.91 | 0.82 |
| Full scan | 0.57 | 0.82 | 0.79 | 0.70 |
| Join + aggregate | 0.09 | 0.61 | 1.2 | 1.1 |
| Read tx (10) | 0.65 | 1.9 | 0.54 | 0.53 |
| Update x1 | 0.64 | 1.0 | 0.42 | 10 |
| Update x10 | 1.5 | 4.3 | 0.61 | 10 |
| Update x100 | 9.1 | 25 | 3.3 | 19 |
| Insert | 0.54 | 0.72 | 0.47 | 9.5 |
| Delete | 0.54 | 0.75 | 0.31 | 9.6 |
| Rollback | 0.32 | 0.42 | 0.05 | 0.05 |

### Single-client SQL latency, 10k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.04 | 0.12 | 0.07 | 0.07 |
| Point miss | 0.04 | 0.14 | 0.06 | 0.06 |
| Range (100) | 0.09 | 0.26 | 0.12 | 0.11 |
| Ordered limit | 0.05 | 0.20 | 7.7 | 8.2 |
| Full scan | 2.6 | 4.4 | 6.7 | 7.1 |
| Join + aggregate | 0.59 | 2.6 | 9.4 | 10 |
| Read tx (10) | 0.73 | 2.1 | 0.52 | 0.52 |
| Update x1 | 0.72 | 0.97 | 0.37 | 18 |
| Update x10 | 1.4 | 3.3 | 0.45 | 19 |
| Update x100 | 8.4 | 25 | 3.0 | 31 |
| Insert | 0.54 | 0.73 | 0.47 | 25 |
| Delete | 0.54 | 0.71 | 0.51 | 19 |
| Rollback | 0.32 | 0.40 | 0.05 | 0.05 |

### Single-client SQL latency, 50k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.04 | 0.14 | 0.07 | 0.07 |
| Point miss | 0.03 | 0.14 | 0.06 | 0.06 |
| Range (100) | 0.15 | 0.26 | 0.11 | 0.11 |
| Ordered limit | 0.06 | 0.21 | 39 | 39 |
| Full scan | 13 | 20 | 34 | 34 |
| Join + aggregate | 2.6 | 12 | 45 | 43 |
| Read tx (10) | 0.94 | 2.1 | 0.71 | 0.58 |
| Update x1 | 0.57 | 1.0 | 0.48 | 41 |
| Update x10 | 1.3 | 3.3 | 0.62 | 41 |
| Update x100 | 8.4 | 25 | 3.0 | 51 |
| Insert | 0.55 | 0.75 | 0.56 | 39 |
| Delete | 0.51 | 0.72 | 0.56 | 39 |
| Rollback | 0.30 | 0.39 | 0.05 | 0.04 |

### Concurrency, 50k rows

Each cell: p50 ms / successful ops per second / share of attempts rejected as conflicts. RepoDB rejects conflicting writers (`ErrConflict`) and the harness does not retry, so p50 covers successful requests only.

| Workload | Clients | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mixed 80/20 read/write | 4 | 0.12 / 18,128 / 0% | 0.42 / 5,909 / 0% | 0.05 / 385 / 2% | 0.08 / 405 / 15% |
| Mixed 80/20 read/write | 16 | 0.17 / 26,206 / 0% | 0.53 / 5,832 / 0% | 0.07 / 190 / 5% | 0.09 / 514 / 19% |
| Contended increment | 4 | 1.7 / 2,231 / 0% | 1.7 / 2,317 / 0% ✗ | 0.48 / 308 / 7% | 41 / 24 / 75% |
| Contended increment | 16 | 6.9 / 2,252 / 0% | 5.4 / 2,378 / 0% ✗ | 0.40 / 162 / 13% | 110 / 9 / 93% |

### Sync and Git operations, 50k rows, p50 ms (RepoDB only)

| Workload | Native Git | Journal |
| --- | ---: | ---: |
| Initial publish + sync | 244 | 941 |
| Publish + sync after writes | 130 | 306 |
| Clone + enable + open | 198 | 174 |
| Sync unchanged | 29 | 89 |
| Edit/sync roundtrip | 220 | 367 |
| Divergent merge | 656 | 1,091 |
| Conflict resolve | 818 | 1,268 |
| Fresh-process reopen | 137 | 206 |

### Correctness failures

- **Dolt 2.3**, 1,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "34"
- **Dolt 2.3**, 1,000 rows, `contended_increment` × 16 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "480", got "32"
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "34"
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (1 errors, 450 successes)
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "35"
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (3 errors, 390 successes)

### Run metadata

| Mode | Started (UTC) | Revision | Clean tree | Server | Peak RSS MiB | Run failure |
| --- | --- | --- | --- | --- | ---: | --- |
| MySQL 8.4 | 2026-10-02T07:18:25 | `636af3d` | yes | 8.4.11 | 38 | no |
| Dolt 2.3 | 2026-10-02T07:18:38 | `636af3d` | yes | Dolt 2.3.5 (VERSION() 8.0.31) | 38 | yes |
| Journal | 2026-10-02T07:15:56 | `636af3d` | yes | — | 276 | no |
| Native Git | 2026-10-02T07:13:54 | `636af3d` | yes | — | 217 | no |

## Reading these numbers

- p50/p95/p99 come from 30 requests per client. Tails are indicative only, and p50s
  of sub-millisecond workloads move by up to ~1.5× between runs at this sample size,
  even for unchanged software.
- "not measured": the harness stops a fixture size at its first hard error. No
  fixture stopped in this run.
- RepoDB runs embedded in the harness process. MySQL and Dolt are reached through
  `go-sql-driver/mysql` over TCP between containers on the same VM, so every
  request includes a client/server round trip that RepoDB's numbers do not. The
  `mysql_point_read` and `mysql_update` rows in the raw JSON measure RepoDB's own
  MySQL-protocol server the same way.
- MySQL and Dolt skip RepoDB's Git-specific workloads (sync, merge, conflict,
  reopen), so the sync table compares only the two RepoDB modes.
- Peak RSS covers the harness process only (`dbbench`, which embeds RepoDB), not
  Git subprocesses or the database containers.
- Absolute numbers depend on the VM: Docker Desktop's virtual disk and network are
  part of every measurement. Compare systems within this file, not with results
  from other machines or setups.
