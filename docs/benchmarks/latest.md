# Latest benchmark results

This file is the one canonical source for RepoDB's current performance. The
methodology and the process for replacing this file are in
[benchmark.md](../benchmark.md). Past reports are in [history/](history/).

**Measured 2026-09-28 at `57a0cd8`** (clean tree), scorecard suite v1 with default
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

From a detached checkout of `57a0cd8`, one `dbbench` binary built with
`go build -tags gms_pure_go ./experiments/dbbench`, run in this order:

```sh
dbbench -mode native-git -output scorecard-native-git.json
dbbench -mode journal    -output scorecard-journal.json
dbbench -mode external -dsn 'root@tcp(127.0.0.1:3306)/' -output scorecard-mysql.json
dbbench -mode external -dsn 'root@tcp(127.0.0.1:3336)/' -output scorecard-dolt.json
```

Wall time: native-git 14 min, journal 50 min, MySQL 10 s, Dolt 13 s.

## Summary

- **Point reads** take 0.07–0.09 ms p50 in both RepoDB modes at every size.
  MySQL takes 0.23–0.44 ms and Dolt 0.28–0.38 ms, but those include a loopback TCP
  round trip, while RepoDB runs in-process. Measured the same way, through RepoDB's
  own MySQL-protocol server over loopback, a journal point read at 50k rows takes
  0.14 ms (`mysql_point_read` in the raw JSON). RepoDB serves reads from an in-memory
  snapshot after cheap on-disk change checks (a `stat` of the journal and of the
  ref storage files).
- **Journal writes** take about 5 ms per single-row transaction (one `fsync`). That
  is slower than MySQL and Dolt (0.7–2.8 ms). Batches amortize: 100 updates take
  7 ms, against 44–51 ms for MySQL and 74–80 ms for Dolt.
- **Native-Git writes** publish a Git snapshot per transaction: 119–319 ms.
- **Range queries and full scans** grow with table size in RepoDB (87–96 ms at 50k
  rows versus 0.3–36 ms for MySQL and Dolt). RepoDB decodes rows from a
  content-addressed tree rather than scanning buffer-pool pages.
- **Contention** is handled by rejecting conflicting writers, not queueing them.
  Under a contended increment, 37–70% of RepoDB attempts are rejected, and
  successful throughput is far below MySQL's.
- **Journal sync** is the slowest path: divergent merge and conflict resolution
  take about 20 s per operation at 50k rows, against 2.3–3.2 s for native-Git.
- **Dolt 2.3.5 failed correctness** under concurrent increments (lost acknowledged
  updates, then serialization errors). Its contended-increment numbers are marked
  ✗ and are not comparable. MySQL, journal and native-Git passed every check.

## Changes since the 2026-09-13 report

The [2026-09-13 scorecard](history/2026-09-13-m4.4-scorecard.md) is not a
like-for-like baseline. Its native-Git run (`559c46a`) predates the M4.4 overhead
work (`864731d`), and its journal run (`c30d2ef`) was M4.4 work in progress with
uncommitted changes. The differences below therefore combine M4.4 with everything
merged since. They are measured, not attributed to individual changes.

- **Native-Git reads** caught up with journal reads: point reads at 50k rows went
  from 52 ms to 0.09 ms, and full scans from 140 ms to 87 ms.
- **Native-Git writes** are somewhat faster: single-row update at 50k rows went
  from 414 to 319 ms, and insert from 209 to 160 ms.
- **Journal merge sync** is about 2.3× slower: divergent merge went from 8.5 to
  19.8 s, and conflict resolve from 9.2 to 20.7 s at 50k rows. The cause is being
  investigated in rdb-59c7d7.
- **Native-Git sync** now has complete numbers. The old run aborted its sync
  workloads (the harness bug fixed in rdb-802f5d).

## Results

### Single-client SQL latency, 1k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.44 | 0.28 | 0.08 | 0.07 |
| Point miss | 0.36 | 0.29 | 0.07 | 0.06 |
| Range (100) | 0.41 | 0.45 | 1.6 | 1.6 |
| Ordered limit | 0.41 | 0.42 | 1.8 | 1.8 |
| Full scan | 15 | 15 | 1.6 | 1.6 |
| Join + aggregate | 0.42 | 1.1 | 2.0 | 1.9 |
| Read tx (10) | 6.5 | 7.4 | 0.60 | 0.56 |
| Update x1 | 2.3 | 1.6 | 5.1 | 153 |
| Update x10 | 8.0 | 7.4 | 5.1 | 134 |
| Update x100 | 51 | 74 | 7.2 | 151 |
| Insert | 0.80 | 0.90 | 5.3 | 119 |
| Delete | 0.84 | 0.92 | 5.5 | 121 |
| Rollback | 0.80 | 0.84 | 0.05 | 0.05 |

### Single-client SQL latency, 10k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.23 | 0.31 | 0.09 | 0.09 |
| Point miss | 0.32 | 0.31 | 0.09 | 0.09 |
| Range (100) | 0.37 | 0.44 | 17 | 17 |
| Ordered limit | 0.33 | 0.40 | 19 | 18 |
| Full scan | 16 | 23 | 17 | 17 |
| Join + aggregate | 0.91 | 3.1 | 19 | 19 |
| Read tx (10) | 6.2 | 7.2 | 0.77 | 0.75 |
| Update x1 | 1.4 | 1.5 | 4.3 | 243 |
| Update x10 | 5.4 | 6.7 | 5.0 | 210 |
| Update x100 | 51 | 76 | 7.5 | 228 |
| Insert | 2.8 | 1.1 | 5.0 | 136 |
| Delete | 2.1 | 1.0 | 5.0 | 135 |
| Rollback | 1.0 | 0.82 | 0.06 | 0.05 |

### Single-client SQL latency, 50k rows, p50 ms

| Workload | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: |
| Point read | 0.26 | 0.38 | 0.09 | 0.09 |
| Point miss | 0.27 | 0.38 | 0.09 | 0.09 |
| Range (100) | 0.39 | 0.55 | 89 | 87 |
| Ordered limit | 0.33 | 0.48 | 94 | 94 |
| Full scan | 28 | 36 | 89 | 87 |
| Join + aggregate | 3.0 | 14 | 96 | 90 |
| Read tx (10) | 6.4 | 7.7 | 0.79 | 0.74 |
| Update x1 | 1.3 | 1.5 | 4.6 | 319 |
| Update x10 | 4.9 | 6.8 | 5.0 | 254 |
| Update x100 | 44 | 80 | 7.0 | 257 |
| Insert | 0.74 | 0.91 | 5.0 | 160 |
| Delete | 0.72 | 1.0 | 5.0 | 160 |
| Rollback | 0.68 | 0.87 | 0.06 | 0.04 |

### Concurrency, 50k rows

Each cell: p50 ms / successful ops per second / share of attempts rejected as conflicts. RepoDB rejects conflicting writers (`ErrConflict`) and the harness does not retry, so p50 covers successful requests only.

| Workload | Clients | MySQL 8.4 | Dolt 2.3 | Journal | Native Git |
| --- | ---: | ---: | ---: | ---: | ---: |
| Mixed 80/20 read/write | 4 | 0.54 / 4,374 / 0% | 0.91 / 3,143 / 0% | 0.08 / 417 / 11% | 0.16 / 20 / 12% |
| Mixed 80/20 read/write | 16 | 1.4 / 9,158 / 0% | not measured | 0.24 / 148 / 7% | 0.16 / 23 / 15% |
| Contended increment | 4 | 1.8 / 2,185 / 0% | 0.91 / 1,951 / 0% ✗ | 4.9 / 112 / 43% | 192 / 5 / 58% |
| Contended increment | 16 | 7.0 / 2,056 / 0% | not measured | 5.8 / 94 / 37% | 185 / 4 / 70% |

### Sync and Git operations, 50k rows, p50 ms (RepoDB only)

| Workload | Native Git | Journal |
| --- | ---: | ---: |
| Initial publish + sync | 462 | 6,641 |
| Publish + sync after writes | 1,950 | 2,965 |
| Clone + enable + open | 633 | 511 |
| Sync unchanged | 165 | 953 |
| Edit/sync roundtrip | 967 | 3,807 |
| Divergent merge | 2,331 | 19,818 |
| Conflict resolve | 3,212 | 20,721 |
| Fresh-process reopen | 329 | 437 |

### Correctness failures

- **Dolt 2.3**, 1,000 rows, `contended_increment` × 4 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "120", got "40"
- **Dolt 2.3**, 1,000 rows, `contended_increment` × 16 clients: lost-update check: SELECT value FROM counter WHERE id = 1: expected "480", got "34"
- **Dolt 2.3**, 10,000 rows, `contended_increment` × 4 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (2 errors, 64 successes)
- **Dolt 2.3**, 50,000 rows, `contended_increment` × 4 clients: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction. (2 errors, 60 successes)

### Run metadata

| Mode | Started (UTC) | Revision | Clean tree | Server | Peak RSS MiB | Run failure |
| --- | --- | --- | --- | --- | ---: | --- |
| MySQL 8.4 | 2026-09-28T12:48:28 | `57a0cd8` | yes | 8.4.11 | 33 | no |
| Dolt 2.3 | 2026-09-28T12:48:45 | `57a0cd8` | yes | Dolt 2.3.5 (VERSION() 8.0.31) | 33 | yes |
| Journal | 2026-09-28T11:58:40 | `57a0cd8` | yes | — | 604 | no |
| Native Git | 2026-09-28T11:44:55 | `57a0cd8` | yes | — | 314 | no |

## Reading these numbers

- p50/p95/p99 come from 30 requests per client. Tails are indicative only; single
  values such as MySQL's 1k-row point read (0.44 ms versus 0.23 ms at 10k rows)
  vary between runs at this sample size.
- "not measured": the harness stops a fixture size at its first hard error. Dolt's
  serialization errors in the 4-client contended increment at 10k and 50k rows
  ended those fixtures before their 16-client workloads.
- RepoDB runs embedded in the harness process. MySQL and Dolt are reached through
  `go-sql-driver/mysql` over loopback TCP, so every request includes a client/server
  round trip that RepoDB's numbers do not. The `mysql_point_read` and `mysql_update`
  rows in the raw JSON measure RepoDB's own MySQL-protocol server the same way.
- MySQL and Dolt skip RepoDB's Git-specific workloads (sync, merge, conflict,
  reopen), so the sync table compares only the two RepoDB modes.
- Peak RSS covers the harness process only (`dbbench`, which embeds RepoDB), not
  Git subprocesses or the database containers.
