# Latest benchmark results

This file is the one canonical source for RepoDB's current performance. The
methodology and the process for replacing this file are in
[benchmark.md](../benchmark.md). Past reports are in [history/](history/).

**Measured 2026-09-28 at `c1aa399`** (clean tree), scorecard suite v1 with default
configuration: 1k/10k/50k rows, 1/4/16 clients, 30 requests per client per
repeated workload. Raw reports: [native-git](latest/scorecard-native-git.json),
[journal](latest/scorecard-journal.json), [MySQL](latest/scorecard-mysql.json),
[Dolt](latest/scorecard-dolt.json).

## Environment

| | |
| --- | --- |
| Machine | Apple M1 Max, 64 GB, on AC power |
| OS / filesystem | macOS 15.8, APFS on the internal SSD (fixtures in the default temp dir) |
| Toolchain | Go 1.27.1, Git 2.55.0, RepoDB built with `-tags gms_pure_go` |
| Baselines | MySQL 8.4.11 (`mysql:8.4`) and Dolt 2.3.5 (`dolthub/dolt-sql-server:latest`) in Docker 29.8.0 |
| Isolation | Runs were sequential; only the system under test was running (both containers stopped during the RepoDB runs, one container at a time otherwise). Ordinary desktop applications stayed open. |

## Commands

From a detached checkout of `c1aa399`, one `dbbench` binary built with
`go build -tags gms_pure_go ./experiments/dbbench`, run in this order:

```sh
dbbench -mode native-git -output scorecard-native-git.json
dbbench -mode journal    -output scorecard-journal.json
dbbench -mode external -dsn 'root@tcp(127.0.0.1:3306)/' -output scorecard-mysql.json
dbbench -mode external -dsn 'root@tcp(127.0.0.1:3336)/' -output scorecard-dolt.json
```

Wall time: native-git 12 min, journal 9 min, MySQL 11 s, Dolt 13 s.

## Summary

- **Point reads** take 0.08–0.11 ms p50 in both RepoDB modes at every size.
  MySQL takes 0.24–0.31 ms and Dolt 0.41–0.42 ms, but those include a loopback TCP
  round trip, while RepoDB runs in-process. Measured the same way, through RepoDB's
  own MySQL-protocol server over loopback, a journal point read at 50k rows takes
  0.14 ms (`mysql_point_read` in the raw JSON).
- **Range reads** on the primary key take 0.10–0.14 ms at every size: they seek into
  the key-ordered tree and decode only the rows they return. MySQL takes 0.29–0.36 ms
  and Dolt 0.49–0.52 ms (again including the TCP round trip).
- **Full scans and joins** still grow with table size: at 50k rows a full scan takes
  32–33 ms (MySQL 26 ms, Dolt 39 ms), and the join, which reads every `bench` row,
  takes 34–36 ms (MySQL 2.9 ms, Dolt 14 ms).
- **Ordered limit** here is `ORDER BY id DESC LIMIT 20`. Reverse index scans are not
  implemented yet (rdb-acd36d), so RepoDB sorts the whole table: 37–39 ms at 50k rows.
  The ascending form is served from the index.
- **Journal writes** take about 5 ms per single-row transaction (4.5–6.1 ms, one
  `fsync`), slower than MySQL and Dolt (1.0–2.2 ms). Batches amortize: 100 updates
  take 7.6–9.0 ms, against 57–58 ms for MySQL and 76–78 ms for Dolt.
- **Native-Git writes** publish a Git snapshot per transaction: 109–260 ms.
- **Contention** is handled by rejecting writers, not queueing them. A write
  transaction is rejected when any other transaction committed after its snapshot,
  even one that wrote different rows. The mixed workload's clients each update
  their own row, yet 6–15% of RepoDB attempts are rejected at 50k rows. Under a
  contended increment, 19–68% are rejected. Successful write throughput is far
  below MySQL's.
- **Sync** at 50k rows: divergent merge takes 1.9 s (native-Git) and 2.6 s
  (journal), conflict resolution 2.2 s and 3.0 s. Journal's slowest operation is
  now the initial publish and sync (7.4 s).
- **Dolt 2.3.5 failed correctness** under concurrent increments (lost acknowledged
  updates, then serialization errors). Its contended-increment numbers are marked
  ✗ and are not comparable. MySQL, journal and native-Git passed every check.

## Changes since `57a0cd8`

The [previous scorecard](history/2026-09-28-57a0cd8-scorecard.md) used the same
machine, environment, suite and commands, so the runs compare directly. Three
changes landed in between: lazy journal replay (rdb-92b9e2), order-preserving keys
with range and ordered-scan pushdown (rdb-586f81), and a binary row encoding
(rdb-ee17d5). MySQL and Dolt did not change, yet some of their sub-millisecond and
single-digit-millisecond p50s moved by up to 1.7× between the runs. That is the
noise level at 30 samples, so only larger RepoDB changes are listed.

- **Range (100)** at 50k rows: 87–89 ms → 0.13–0.14 ms. Range predicates on the
  primary key now seek into the tree (rdb-586f81) instead of scanning it.
- **Full scan** at 50k rows: 87–89 ms → 32–33 ms, with the same ratio (about 0.4×)
  at 1k and 10k rows. **Join + aggregate** (96/90 → 36/34 ms) and the descending
  **ordered limit** (94 → 37–39 ms) scan the whole table too, so they improved by the
  same factor. The scan now streams rows from the tree (rdb-586f81) and decodes
  binary rows instead of JSON (rdb-ee17d5).
- **Journal sync** at 50k rows: divergent merge 19.8 → 2.6 s, conflict resolution
  20.7 → 3.0 s, edit/sync round trip 3.8 → 1.2 s, publish and sync after writes
  3.0 → 1.3 s, sync unchanged 953 → 423 ms. Profiling after the previous run
  (rdb-6ae82d) traced journal sync time to replay loading a snapshot for every
  historical checkpoint, which rdb-92b9e2 removed. Divergent merge is now also well
  below the 2026-09-13 figure (8.5 s) that rdb-59c7d7 was opened against.
- **Fresh-process reopen** at 50k rows: 437 → 266 ms (journal) and 329 → 199 ms
  (native-Git).
- **Peak RSS** of the harness: journal 604 → 360 MiB, native-Git 314 → 231 MiB.
- **Wall time** of the journal run: 50 → 9 min.

## Results

### Single-client SQL latency, 1k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.24 | 0.42 | 0.08 | 0.08 |
| Point miss | 0.22 | 0.35 | 0.08 | 0.08 |
| Range (100) | 0.29 | 0.49 | 0.10 | 0.10 |
| Ordered limit | 0.23 | 0.43 | 0.76 | 0.76 |
| Full scan | 15 | 16 | 0.63 | 0.62 |
| Join + aggregate | 0.50 | 1.1 | 0.79 | 0.85 |
| Read tx (10) | 5.9 | 8.0 | 0.68 | 0.66 |
| Update x1 | 1.6 | 2.2 | 4.9 | 109 |
| Update x10 | 6.6 | 9.3 | 5.1 | 142 |
| Update x100 | 58 | 77 | 8.3 | 147 |
| Insert | 0.98 | 1.2 | 5.2 | 112 |
| Delete | 0.99 | 1.2 | 5.8 | 113 |
| Rollback | 0.98 | 1.1 | 0.05 | 0.05 |

### Single-client SQL latency, 10k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.31 | 0.41 | 0.11 | 0.09 |
| Point miss | 0.28 | 0.38 | 0.10 | 0.09 |
| Range (100) | 0.36 | 0.52 | 0.12 | 0.11 |
| Ordered limit | 0.26 | 0.44 | 8.0 | 7.7 |
| Full scan | 16 | 26 | 6.8 | 6.5 |
| Join + aggregate | 1.0 | 3.5 | 7.5 | 7.1 |
| Read tx (10) | 5.9 | 7.2 | 0.81 | 0.79 |
| Update x1 | 1.6 | 1.8 | 5.1 | 142 |
| Update x10 | 6.6 | 8.7 | 5.1 | 183 |
| Update x100 | 58 | 76 | 7.6 | 198 |
| Insert | 1.0 | 1.3 | 5.1 | 131 |
| Delete | 1.0 | 1.2 | 6.1 | 134 |
| Rollback | 0.97 | 1.1 | 0.05 | 0.05 |

### Single-client SQL latency, 50k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.30 | 0.41 | 0.11 | 0.09 |
| Point miss | 0.24 | 0.41 | 0.10 | 0.10 |
| Range (100) | 0.36 | 0.52 | 0.14 | 0.13 |
| Ordered limit | 0.30 | 0.46 | 39 | 37 |
| Full scan | 26 | 39 | 33 | 32 |
| Join + aggregate | 2.9 | 14 | 36 | 34 |
| Read tx (10) | 5.9 | 6.7 | 0.83 | 0.81 |
| Update x1 | 1.5 | 1.9 | 5.0 | 196 |
| Update x10 | 7.0 | 8.9 | 5.4 | 260 |
| Update x100 | 57 | 78 | 9.0 | 239 |
| Insert | 0.99 | 1.2 | 5.2 | 155 |
| Delete | 1.0 | 1.1 | 4.5 | 160 |
| Rollback | 0.99 | 1.0 | 0.04 | 0.04 |

### Concurrency, 50k rows

Each cell: p50 ms / successful ops per second / share of attempts rejected as conflicts. RepoDB rejects conflicting writers (`ErrConflict`) and the harness does not retry, so p50 covers successful requests only.

| Workload | Clients | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mixed 80/20 read/write | 4 | 0.65 / 5,272 / 0% | 0.89 / 3,558 / 0% | 0.09 / 291 / 6% | 0.14 / 22 / 11% |
| Mixed 80/20 read/write | 16 | 1.5 / 7,562 / 0% | 1.5 / 3,695 / 0% | 0.51 / 195 / 9% | 0.14 / 26 / 15% |
| Contended increment | 4 | 1.8 / 2,227 / 0% | 1.5 / 2,525 / 0% ✗ | 4.7 / 157 / 19% | 182 / 4 / 64% |
| Contended increment | 16 | 6.7 / 2,370 / 0% | 6.6 / 2,154 / 0% ✗ | 5.3 / 128 / 43% | 180 / 4 / 68% |

### Sync and Git operations, 50k rows, p50 ms (RepoDB only)

| Workload | Native Git | Journal |
| --- | ---: | ---: |
| Initial publish + sync | 420 | 7,392 |
| Publish + sync after writes | 1,474 | 1,270 |
| Clone + enable + open | 573 | 441 |
| Sync unchanged | 151 | 423 |
| Edit/sync roundtrip | 843 | 1,167 |
| Divergent merge | 1,853 | 2,567 |
| Conflict resolve | 2,203 | 3,001 |
| Fresh-process reopen | 199 | 266 |

### Correctness failures

- **Dolt 2.3**, 1,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "40"
- **Dolt 2.3**, 1,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (14 errors, 60 successes)
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "42"
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (7 errors, 396 successes)
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "41"
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 16 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (1 errors, 450 successes)

### Run metadata

| Mode | Started (UTC) | Revision | Clean tree | Server | Peak RSS MiB | Run failure |
| --- | --- | --- | --- | --- | ---: | --- |
| MySQL 8.4 | 2026-09-28T16:14:45 | `c1aa399` | yes | 8.4.11 | 33 | no |
| Dolt 2.3 | 2026-09-28T16:15:10 | `c1aa399` | yes | Dolt 2.3.5 (VERSION() 8.0.31) | 34 | yes |
| Journal | 2026-09-28T16:05:19 | `c1aa399` | yes | — | 360 | no |
| Native Git | 2026-09-28T15:53:30 | `c1aa399` | yes | — | 231 | no |

## Reading these numbers

- p50/p95/p99 come from 30 requests per client. Tails are indicative only, and even
  p50s of sub-millisecond workloads vary between runs at this sample size: see the
  MySQL and Dolt deltas under "Changes since `57a0cd8`".
- "not measured": the harness stops a fixture size at its first hard error. No
  fixture stopped in this run.
- RepoDB runs embedded in the harness process. MySQL and Dolt are reached through
  `go-sql-driver/mysql` over loopback TCP, so every request includes a client/server
  round trip that RepoDB's numbers do not. The `mysql_point_read` and `mysql_update`
  rows in the raw JSON measure RepoDB's own MySQL-protocol server the same way.
- MySQL and Dolt skip RepoDB's Git-specific workloads (sync, merge, conflict,
  reopen), so the sync table compares only the two RepoDB modes.
- Peak RSS covers the harness process only (`dbbench`, which embeds RepoDB), not
  Git subprocesses or the database containers.
