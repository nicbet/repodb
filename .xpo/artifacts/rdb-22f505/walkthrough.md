# Walkthrough: dbbench server version and correctness-failure listing

Two small reporting changes to `experiments/dbbench`, so that published scorecards (rdb-6ae82d) can name what they measured and can't present a failed row as a pass.

**Server version.** `report.Server` (`server_version` in JSON) is filled in external mode by `externalServerVersion`, which opens a short-lived connection without a database, reads `SELECT VERSION()`, and then tries `SELECT dolt_version()`. Dolt's `VERSION()` is the MySQL version it emulates (8.0.31), so for Dolt the value reads `Dolt 2.3.5 (VERSION() 8.0.31)`. A failure to read it becomes a run failure but doesn't stop the run. The header prints `server …` when set.

**Correctness failures.** The Status column already said FAIL for rows with errors or a failed verification, but never why. `printReport` now lists, after the table, one line per such row: rows, workload, clients, and the verification error, or the first error plus a count of the rest.

Workloads, suite version (1) and existing JSON fields are unchanged, so reports stay comparable.
