# Latest benchmark results

This file is the one canonical source for RepoDB's current performance. The
methodology, the setup and the process for replacing this file are in
[benchmark.md](../benchmark.md). Earlier results are in this file's Git history
(`git log -p docs/benchmarks/latest.md`).

**Measured 2026-10-02 at `6b76e87`** (clean tree), scorecard suite v1 with default
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

From a clean checkout of `6b76e87`, with the baseline containers attached to the
`repodb-bench` network and started one at a time:

```sh
make bench-docker BENCH_MODE=native-git
make bench-docker BENCH_MODE=journal
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-mysql BENCH_DSN='root@tcp(repodb-bench-mysql:3306)/'
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-dolt  BENCH_DSN='root@tcp(repodb-bench-dolt:3306)/'
```

Wall time: native-git 1.5 min, journal 1.4 min, MySQL 5 s, Dolt 7 s.

## Summary

All figures are p50; ranges span 1k–50k rows unless a size is named.

- **Reads** are on par with or faster than MySQL's, except joins:
  - point reads take 0.03–0.04 ms (MySQL 0.04–0.07 ms, Dolt 0.15–0.16 ms);
  - ranges of 100 rows 0.06–0.07 ms (MySQL 0.09–0.14 ms);
  - a transaction of ten point reads 0.25–0.34 ms (MySQL 0.68–0.96 ms);
  - the descending ordered limit (`ORDER BY id DESC LIMIT 20`) 0.04–0.09 ms (MySQL 0.04–0.06 ms);
  - a full scan of 50k rows 13 ms, the same as MySQL (Dolt 21 ms).

  All of these include a TCP round trip for MySQL and Dolt but not for embedded
  RepoDB; through RepoDB's own MySQL-protocol server, a journal point read at 50k
  rows takes 0.05 ms (`mysql_point_read` in the raw JSON).
- **The join** (`… JOIN authors a ON a.id = 1 GROUP BY a.name`) takes 15–17 ms at 50k
  rows (MySQL 2.6 ms, Dolt 12 ms). RepoDB no longer decodes any `bench` row for it.
  What remains is go-mysql-server grouping and joining row by row, where MySQL
  resolves the join to a constant and counts.
- **Single-row writes (journal)** take 0.28–0.40 ms (MySQL 0.52–0.89 ms, Dolt
  0.65–1.0 ms), and 100-row batches 2.8–3.7 ms (MySQL 8.5–9.1 ms, Dolt 26 ms).
- **Native-Git writes** publish a Git snapshot per transaction: 7.8–8.2 ms at 1k rows,
  14–17 ms at 50k rows.
- **Contention** is handled by rejecting writers, not queueing them: a write
  transaction is rejected when any other transaction committed after its snapshot,
  even one that wrote different rows. In the mixed workload, where each client
  updates its own row, 4% (journal) and 15–18% (native Git) of attempts are
  rejected at 50k rows; under a contended increment, 8–15% and 72–86%. Successful
  write throughput is far below MySQL's.
- **Sync** at 50k rows: divergent merge takes 0.31 s (native Git) and 0.54 s
  (journal), conflict resolution 0.37 s and 0.64 s; the journal's initial publish
  and sync of the bulk-loaded table takes 0.57 s.
- **Dolt 2.3.5 failed correctness** under concurrent increments (lost acknowledged
  updates, serialization errors). Its contended-increment numbers are marked ✗ and
  are not comparable. MySQL, journal and native Git passed every check.

## Changes since `636af3d`

Same setup, machine and baselines as the previous run. The MySQL and Dolt reruns
measure the noise: their p50s moved by 0.92–1.34× (MySQL) and 0.85–1.14× (Dolt)
for 80% of workloads, and by up to 0.6–2.4× at the extremes. Only RepoDB changes
well beyond that are listed, at 50k rows, with the issues responsible:

- **Full scan** 34 → 13 ms in both modes, and **join** 45 → 15 ms (journal) and
  43 → 17 ms (native Git). Prolly nodes are binary and read in place instead of
  JSON with base64 (format 5); queries decode only the columns they use, so the
  join decodes no `bench` row (rdb-48f066).
- **Ordered limit** 39 → 0.04 ms: `ORDER BY id DESC LIMIT 20` scans the index
  backwards instead of sorting the table (rdb-acd36d).
- **Point reads** 0.075 → 0.03–0.04 ms, **ranges** 0.11 → 0.06–0.07 ms and **read
  transactions** 0.58–0.71 → 0.25–0.26 ms: no JSON decoding and no copy of each
  cached node (rdb-48f066).
- **Native-Git writes** 39–51 → 14–17 ms, with **bulk load** 555 → 439 ms. Chunk
  boundaries now come from a mixed hash with larger chunks, so a 50k-row table has
  about 400 leaves instead of 742, trees are shallower, and each commit writes
  fewer and smaller Git objects (rdb-a0d510, format 6; rdb-48f066).
- **Sync and merge** roughly halved in both modes: divergent merge 0.66 → 0.31 s
  (native Git) and 1.09 → 0.54 s (journal), conflict resolution 0.82 → 0.37 s and
  1.27 → 0.64 s, fresh-process reopen 137 → 56 ms and 206 → 87 ms, and the journal's
  initial publish 0.94 → 0.57 s. Fewer, smaller objects to write, read and verify
  (rdb-48f066, rdb-a0d510).
- **Journal writes** with uncheckpointed edits no longer slow down as edits
  accumulate: a commit appends only its own edits, and secondary indexes share the
  pending entries instead of copying them (rdb-a6a4d2, rdb-24820a, rdb-6bd369). The
  scorecard checkpoints before its write workloads, so these numbers show the
  effect only modestly (single-row update 0.48 → 0.34 ms).
- **Peak RSS** of the harness: native Git 217 → 140 MiB, journal 276 → 350 MiB. A
  single sample each; the journal increase is not yet explained.

Formats 4 and 5 are refused by this revision with the unsupported-format error;
RepoDB is alpha and does not migrate data between formats.

## Results

### Single-client SQL latency, 1k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.07 | 0.15 | 0.04 | 0.04 |
| Point miss | 0.08 | 0.16 | 0.03 | 0.03 |
| Range (100) | 0.09 | 0.27 | 0.06 | 0.07 |
| Ordered limit | 0.04 | 0.16 | 0.04 | 0.04 |
| Full scan | 0.52 | 0.89 | 0.29 | 0.24 |
| Join + aggregate | 0.10 | 0.56 | 0.43 | 0.54 |
| Read tx (10) | 0.77 | 2.1 | 0.34 | 0.29 |
| Update x1 | 0.89 | 1.0 | 0.28 | 8.2 |
| Update x10 | 1.8 | 3.4 | 0.66 | 8.2 |
| Update x100 | 9.1 | 26 | 3.7 | 11 |
| Insert | 0.53 | 0.72 | 0.31 | 7.8 |
| Delete | 0.53 | 0.73 | 0.36 | 7.8 |
| Rollback | 0.32 | 0.40 | 0.04 | 0.04 |

### Single-client SQL latency, 10k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.04 | 0.16 | 0.03 | 0.03 |
| Point miss | 0.04 | 0.13 | 0.03 | 0.03 |
| Range (100) | 0.14 | 0.25 | 0.06 | 0.06 |
| Ordered limit | 0.06 | 0.21 | 0.09 | 0.04 |
| Full scan | 2.6 | 4.6 | 2.8 | 2.5 |
| Join + aggregate | 0.60 | 2.8 | 3.2 | 3.3 |
| Read tx (10) | 0.96 | 2.0 | 0.25 | 0.25 |
| Update x1 | 0.63 | 1.0 | 0.36 | 10 |
| Update x10 | 1.4 | 3.4 | 0.60 | 10 |
| Update x100 | 8.5 | 26 | 3.0 | 12 |
| Insert | 0.52 | 0.65 | 0.37 | 9.8 |
| Delete | 0.54 | 0.77 | 0.33 | 9.4 |
| Rollback | 0.30 | 0.39 | 0.04 | 0.04 |

### Single-client SQL latency, 50k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.04 | 0.16 | 0.04 | 0.03 |
| Point miss | 0.03 | 0.14 | 0.03 | 0.03 |
| Range (100) | 0.10 | 0.26 | 0.07 | 0.06 |
| Ordered limit | 0.04 | 0.21 | 0.04 | 0.04 |
| Full scan | 13 | 21 | 13 | 13 |
| Join + aggregate | 2.6 | 12 | 15 | 17 |
| Read tx (10) | 0.68 | 2.2 | 0.26 | 0.25 |
| Update x1 | 0.69 | 1.0 | 0.34 | 14 |
| Update x10 | 1.4 | 3.3 | 0.49 | 14 |
| Update x100 | 8.7 | 26 | 2.8 | 17 |
| Insert | 0.54 | 0.83 | 0.40 | 14 |
| Delete | 0.53 | 0.80 | 0.38 | 14 |
| Rollback | 0.31 | 0.41 | 0.04 | 0.04 |

### Concurrency, 50k rows

Each cell: p50 ms / successful ops per second / share of attempts rejected as conflicts. RepoDB rejects conflicting writers (`ErrConflict`) and the harness does not retry, so p50 covers successful requests only.

| Workload | Clients | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mixed 80/20 read/write | 4 | 0.12 / 14,735 / 0% | 0.45 / 5,640 / 0% | 0.04 / 1,057 / 4% | 0.04 / 879 / 15% |
| Mixed 80/20 read/write | 16 | 0.19 / 34,996 / 0% | 0.61 / 4,845 / 0% | 0.06 / 666 / 4% | 0.05 / 1,323 / 18% |
| Contended increment | 4 | 1.7 / 2,211 / 0% | 1.7 / 2,264 / 0% ✗ | 0.41 / 700 / 8% | 14 / 66 / 72% |
| Contended increment | 16 | 7.0 / 2,222 / 0% | 5.5 / 2,334 / 0% ✗ | 0.33 / 388 / 15% | 16 / 56 / 86% |

### Sync and Git operations, 50k rows, p50 ms (RepoDB only)

| Workload | Native Git | Journal |
| --- | ---: | ---: |
| Initial publish + sync | 168 | 571 |
| Publish + sync after writes | 107 | 151 |
| Clone + enable + open | 128 | 97 |
| Sync unchanged | 23 | 65 |
| Edit/sync roundtrip | 131 | 208 |
| Divergent merge | 315 | 542 |
| Conflict resolve | 373 | 639 |
| Fresh-process reopen | 56 | 87 |

### Correctness failures

- **Dolt 2.3**, 1,000 rows, `contended_increment` × 4 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (1 errors, 90 successes)
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "33"
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (2 errors, 420 successes)
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "34"
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (4 errors, 385 successes)

### Run metadata

| Mode | Started (UTC) | Revision | Clean tree | Server | Peak RSS MiB | Run failure |
| --- | --- | --- | --- | --- | ---: | --- |
| MySQL 8.4 | 2026-10-02T10:00:18 | `6b76e87` | yes | 8.4.11 | 36 | no |
| Dolt 2.3 | 2026-10-02T10:00:32 | `6b76e87` | yes | Dolt 2.3.5 (VERSION() 8.0.31) | 38 | yes |
| Journal | 2026-10-02T09:58:50 | `6b76e87` | yes | — | 350 | no |
| Native Git | 2026-10-02T09:57:41 | `6b76e87` | yes | — | 140 | no |

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
