# dbbench: record external server version and list correctness failures

## What / Why

rdb-6ae82d must state the MySQL and Dolt versions in latest.md, and must not let a failed-correctness row read as a comparable result. Today the JSON has no server version, and the printed table shows `FAIL` without saying why.

## How

- `report` gains `ServerVersion string json:"server_version,omitempty"`. `runExternal` fills it from `SELECT VERSION()` through a short-lived admin connection (`cfg.DBName = ""`) before the first fixture. A failure to read it is recorded as a run failure, but does not stop the run.
- `printReport` prints `server <version>` in the header when set. After the table, it prints a "Correctness failures:" section with one line per row that has a `verification_error` or errors: rows, workload, clients, then the verification error or the first error plus a count.
- Nothing else changes: workloads, suite version (still 1), and existing JSON fields.

## Acceptance criteria

- Smoke runs against MySQL 8.4 and Dolt 2.3.5 show the server version in the header and JSON. The Dolt run lists its lost-update failure under the table.
- `make lint` and `make test` pass.
