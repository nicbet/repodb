# Latest benchmark results

This file is the one canonical source for RepoDB's current performance. The
methodology and the process for replacing this file are in
[benchmark.md](../benchmark.md). Earlier results are in this file's Git history
(`git log -p docs/benchmarks/latest.md`).

**Measured 2026-10-02 at `87dd885`** (clean tree), scorecard suite v1 with default
configuration: 1k/10k/50k rows, 1/4/16 clients, 30 requests per client per
repeated workload. Raw reports: [native-git](latest/scorecard-native-git.json),
[journal](latest/scorecard-journal.json), [MySQL](latest/scorecard-mysql.json),
[Dolt](latest/scorecard-dolt.json).

Journal is RepoDB's default persistence mode (library, `repodb start` and
`repodb-server`). Native Git is the opt-in audit mode, in which every transaction
is a Git data commit.

## Environment

| | |
| --- | --- |
| Machine | Apple M1 Max, 64 GB, on AC power |
| OS / filesystem | macOS 15.8, APFS on the internal SSD (fixtures in the default temp dir) |
| Toolchain | Go 1.27.1, Git 2.55.0, RepoDB built with `-tags gms_pure_go` |
| Baselines | MySQL 8.4.11 (`mysql:8.4`) and Dolt 2.3.5 (`dolthub/dolt-sql-server:latest`) in Docker 29.8.1 |
| Isolation | Runs were sequential; only the system under test was running (unrelated containers and both baseline containers stopped during the RepoDB runs, one baseline container at a time otherwise). Ordinary desktop applications stayed open. |
| Git housekeeping | Fixture repositories disable automatic gc and maintenance; `git gc` runs untimed between workload groups (`"git_gc": "manual"`, see [benchmark.md](../benchmark.md)). |

## Commands

From a clean checkout of `87dd885`, one `dbbench` binary built with
`go build -tags gms_pure_go ./experiments/dbbench`, run in this order:

```sh
dbbench -mode native-git -output scorecard-native-git.json
dbbench -mode journal    -output scorecard-journal.json
dbbench -mode external -dsn 'root@tcp(127.0.0.1:3306)/' -output scorecard-mysql.json
dbbench -mode external -dsn 'root@tcp(127.0.0.1:3336)/' -output scorecard-dolt.json
```

Wall time: native-git 10 min, journal 10 min, MySQL 9 s, Dolt 13 s.

## Summary

- **Point reads** take 0.08–0.09 ms p50 in both RepoDB modes at every size.
  MySQL takes 0.21–0.22 ms and Dolt 0.28–0.38 ms, but those include a loopback TCP
  round trip, while RepoDB runs in-process. Measured the same way, through RepoDB's
  own MySQL-protocol server over loopback, a journal point read at 50k rows takes
  0.13 ms (`mysql_point_read` in the raw JSON).
- **Range reads** on the primary key take 0.10–0.14 ms at every size: they seek into
  the key-ordered tree and decode only the rows they return. MySQL takes 0.33–0.38 ms
  and Dolt 0.48–0.55 ms (again including the TCP round trip).
- **Full scans and joins** still grow with table size: at 50k rows a full scan takes
  35 ms (MySQL 27 ms, Dolt 38 ms), and the join, which reads every `bench` row,
  takes 39 ms (MySQL 3.0 ms, Dolt 13 ms).
- **Ordered limit** here is `ORDER BY id DESC LIMIT 20`. Reverse index scans are not
  implemented yet (rdb-acd36d), so RepoDB sorts the whole table: 40–41 ms at 50k rows.
  The ascending form is served from the index.
- **Journal writes** take about 5 ms per single-row transaction (4.9–5.5 ms, one
  `fsync`), slower than MySQL and Dolt (0.6–1.5 ms). Batches amortize: 100 updates
  take 7.3–8.1 ms, against 42–46 ms for MySQL and 73–77 ms for Dolt.
- **Native-Git writes** publish a Git snapshot per transaction: 119–196 ms p50, with
  tails close to the median (max 206 ms).
- **Contention** is handled by rejecting writers, not queueing them. A write
  transaction is rejected when any other transaction committed after its snapshot,
  even one that wrote different rows. The mixed workload's clients each update
  their own row, yet 8% (journal) and 15–19% (native-Git) of attempts are rejected
  at 50k rows. Under a contended increment, 26–32% (journal) and 75–94% (native-Git)
  are rejected. Successful write throughput is far below MySQL's.
- **Sync** at 50k rows: divergent merge takes 2.1 s (native-Git) and 3.0 s
  (journal), conflict resolution 2.5 s and 2.8 s. Journal's slowest operation is
  the initial publish and sync of the bulk-loaded table (9.0 s).
- **Dolt 2.3.5 failed correctness** under concurrent increments (lost acknowledged
  updates and serialization errors). Its contended-increment numbers are marked ✗
  and are not comparable. MySQL, journal and native-Git passed every check.

## Changes since `c1aa399`

The previous scorecard (this file as of commit `a05608c`) used the same machine,
environment, suite, baselines and commands, except Docker 29.8.0 → 29.8.1. The
RepoDB changes in between include content-local Prolly chunk boundaries with
storage format 4 (rdb-fb3d12), Git housekeeping kept out of timed workloads
(rdb-2124bc), journal compaction at checkpoint (rdb-4d677f), the publication guard
against head moves under a dirty journal (rdb-e0c717) with its contention fix
(rdb-162912), and journal as the default mode (rdb-c56142, no effect on the
harness).

Noise: MySQL and Dolt did not change, yet some of their p50s moved by up to 1.5×.
A first run of this refresh at `f944eee` (which lacked only rdb-162912) also shows
that single-client Git-heavy RepoDB workloads (native-Git writes, syncs) varied by
up to ~25% between the two runs on identical code paths. Only larger changes are
listed.

- **Native-Git write tails are gone.** At 50k rows, Update x10 went from
  260 ms p50 / 1,415 ms p95 / 1,832 ms max to 171 / 182 / 184 ms, and Update x1
  and x100 lost their outliers too (p95 264 → 182 ms and 431 → 202 ms). Small edits
  used to rewrite up to half of a table's Prolly tree, because chunk boundaries
  depended on values and on all earlier entries; boundaries now depend on each key
  alone (rdb-fb3d12).
- **Native-Git mixed read/write throughput**: 22 → 98 successful ops/s with 4
  clients and 26 → 318 with 16. Rejected writers now fail before writing any Git
  object instead of holding the publication lock for a doomed commit (rdb-162912),
  and each commit writes fewer objects (rdb-fb3d12). The `f944eee` run, without
  rdb-162912, measured 37 and 36 ops/s.
- **Native-Git contended increment**: 4 → 7 ops/s (4 clients) and 4 → 6 ops/s (16
  clients), while the rejected share rose from 64% to 75% and from 68% to 94%.
  Writers now wait in order, so each round of simultaneous attempts has exactly one
  winner (1 − 1/clients rejected); before, a client that had just been rejected
  could jump the queue with a fresh snapshot. Fewer rejections need per-key
  conflict detection (rdb-df092b).
- **Publish and sync after writes** at 50k rows: 1,474 → 406 ms (native-Git) and
  1,270 → 837 ms (journal). Journal sync tails shrank: edit/sync round trip max
  2,901 → 1,266 ms, divergent merge max 5,809 → 3,224 ms. Git's automatic
  maintenance used to run, detached, after fetches and pushes and overlap timed
  requests (rdb-2124bc), and checkpoints write fewer objects (rdb-fb3d12).
- **Journal initial publish + sync** at 50k rows got slower: 7.4 → 9.0 s (8.3 s in
  the `f944eee` run; one sample per run). Content-local boundaries produce smaller
  chunks for these keys (65 instead of 78 rows per tree node), so the bulk-loaded
  table's first checkpoint writes about 20% more Git objects. Tuning the chunk size
  is tracked in rdb-a0d510.
- **Wall time** of the native-Git run: 12 → 10 min.

## Results

### Single-client SQL latency, 1k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.21 | 0.30 | 0.08 | 0.08 |
| Point miss | 0.21 | 0.30 | 0.06 | 0.05 |
| Range (100) | 0.32 | 0.55 | 0.11 | 0.10 |
| Ordered limit | 0.32 | 0.47 | 0.83 | 0.76 |
| Full scan | 15 | 15 | 0.68 | 0.64 |
| Join + aggregate | 0.64 | 1.4 | 0.89 | 0.82 |
| Read tx (10) | 6.2 | 7.3 | 0.50 | 0.44 |
| Update x1 | 1.1 | 1.5 | 4.9 | 119 |
| Update x10 | 4.8 | 6.6 | 5.1 | 120 |
| Update x100 | 42 | 77 | 7.4 | 148 |
| Insert | 0.64 | 0.98 | 5.0 | 126 |
| Delete | 0.69 | 0.93 | 5.5 | 127 |
| Rollback | 0.67 | 0.80 | 0.05 | 0.05 |

### Single-client SQL latency, 10k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.21 | 0.28 | 0.09 | 0.08 |
| Point miss | 0.20 | 0.37 | 0.07 | 0.07 |
| Range (100) | 0.36 | 0.48 | 0.13 | 0.14 |
| Ordered limit | 0.29 | 0.42 | 8.5 | 8.2 |
| Full scan | 15 | 26 | 7.1 | 7.0 |
| Join + aggregate | 0.99 | 3.6 | 8.2 | 8.1 |
| Read tx (10) | 6.0 | 7.2 | 0.58 | 0.58 |
| Update x1 | 1.2 | 1.5 | 5.0 | 145 |
| Update x10 | 4.9 | 6.6 | 5.1 | 146 |
| Update x100 | 46 | 74 | 8.1 | 169 |
| Insert | 0.74 | 0.94 | 5.3 | 148 |
| Delete | 0.75 | 0.93 | 5.5 | 145 |
| Rollback | 0.62 | 0.80 | 0.06 | 0.05 |

### Single-client SQL latency, 50k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.22 | 0.38 | 0.09 | 0.09 |
| Point miss | 0.20 | 0.40 | 0.08 | 0.07 |
| Range (100) | 0.38 | 0.49 | 0.13 | 0.14 |
| Ordered limit | 0.28 | 0.40 | 41 | 40 |
| Full scan | 27 | 38 | 35 | 35 |
| Join + aggregate | 3.0 | 13 | 39 | 39 |
| Read tx (10) | 6.2 | 7.0 | 0.60 | 0.60 |
| Update x1 | 1.1 | 1.5 | 5.1 | 172 |
| Update x10 | 4.9 | 6.4 | 5.3 | 171 |
| Update x100 | 44 | 73 | 7.3 | 196 |
| Insert | 0.71 | 0.92 | 5.0 | 172 |
| Delete | 0.67 | 0.93 | 5.0 | 175 |
| Rollback | 0.67 | 0.94 | 0.06 | 0.05 |

### Concurrency, 50k rows

Each cell: p50 ms / successful ops per second / share of attempts rejected as conflicts. RepoDB rejects conflicting writers (`ErrConflict`) and the harness does not retry, so p50 covers successful requests only.

| Workload | Clients | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mixed 80/20 read/write | 4 | 0.63 / 4,923 / 0% | 0.85 / 3,790 / 0% | 0.09 / 359 / 8% | 0.11 / 98 / 15% |
| Mixed 80/20 read/write | 16 | 1.3 / 8,248 / 0% | 1.5 / 3,903 / 0% | 0.25 / 208 / 8% | 0.11 / 318 / 19% |
| Contended increment | 4 | 1.6 / 2,472 / 0% | 1.6 / 2,463 / 0% ✗ | 5.1 / 136 / 26% | 132 / 7 / 75% |
| Contended increment | 16 | 6.6 / 2,408 / 0% | 5.2 / 2,587 / 0% ✗ | 5.5 / 101 / 32% | 161 / 6 / 94% |

### Sync and Git operations, 50k rows, p50 ms (RepoDB only)

| Workload | Native Git | Journal |
| --- | ---: | ---: |
| Initial publish + sync | 531 | 9,006 |
| Publish + sync after writes | 406 | 837 |
| Clone + enable + open | 532 | 388 |
| Sync unchanged | 177 | 300 |
| Edit/sync roundtrip | 927 | 1,225 |
| Divergent merge | 2,087 | 2,986 |
| Conflict resolve | 2,512 | 2,821 |
| Fresh-process reopen | 234 | 294 |

### Correctness failures

- **Dolt 2.3**, 1,000 rows, `contended_increment` × 4 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (2 errors, 79 successes)
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "40"
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 16 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "480", got "33"
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "40"
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (2 errors, 420 successes)

### Run metadata

| Mode | Started (UTC) | Revision | Clean tree | Server | Peak RSS MiB | Run failure |
| --- | --- | --- | --- | --- | ---: | --- |
| MySQL 8.4 | 2026-10-02T06:08:33 | `87dd885` | yes | 8.4.11 | 33 | no |
| Dolt 2.3 | 2026-10-02T06:09:47 | `87dd885` | yes | Dolt 2.3.5 (VERSION() 8.0.31) | 33 | yes |
| Journal | 2026-10-02T05:58:39 | `87dd885` | yes | — | 371 | no |
| Native Git | 2026-10-02T05:48:50 | `87dd885` | yes | — | 233 | no |

## Reading these numbers

- p50/p95/p99 come from 30 requests per client. Tails are indicative only, and even
  p50s vary between runs at this sample size: see the noise note under "Changes
  since `c1aa399`".
- "not measured": the harness stops a fixture size at its first hard error. Dolt's
  1k-row fixture stopped at its contended-increment error with 4 clients, so its
  1k-row 16-client concurrency workloads were not measured; the concurrency table
  above uses 50k rows, where every workload ran.
- RepoDB runs embedded in the harness process. MySQL and Dolt are reached through
  `go-sql-driver/mysql` over loopback TCP, so every request includes a client/server
  round trip that RepoDB's numbers do not. The `mysql_point_read` and `mysql_update`
  rows in the raw JSON measure RepoDB's own MySQL-protocol server the same way.
- MySQL and Dolt skip RepoDB's Git-specific workloads (sync, merge, conflict,
  reopen), so the sync table compares only the two RepoDB modes.
- Peak RSS covers the harness process only (`dbbench`, which embeds RepoDB), not
  Git subprocesses or the database containers.
