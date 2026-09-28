# Refresh latest.md at c1aa399

## What

Run the scorecard (suite v1, default config: 1k/10k/50k rows, 1/4/16 clients, 30 requests) in all four modes at `main` `c1aa399`, then publish it following benchmark.md § Publishing results. Since the current latest.md (`57a0cd8`), three changes have landed: rdb-92b9e2 (lazy journal replay), rdb-586f81 (order-preserving keys with range/ORDER BY pushdown) and rdb-ee17d5 (binary rows).

## Run controls (same as rdb-6ae82d)

- **Clean revision.** Run from a detached `git worktree` of `c1aa399` in the scratchpad, with one `dbbench` binary built with `-tags gms_pure_go`. Every JSON must record `c1aa399…` and an empty `working_tree_status`.
- **Sequential runs:** native-git, journal, MySQL (`mysql:8.4`), then Dolt (`dolthub/dolt-sql-server:latest`, as already pulled locally). Only the system under test runs. Both containers are stopped during the RepoDB runs.
- The environment (machine, OS, filesystem, Go, Git, Docker, server versions) is recorded again, not copied from the previous report.

## Publishing

1. `git mv` the current latest.md to `history/2026-09-28-57a0cd8-scorecard.md`, and its JSON directory to `history/2026-09-28-57a0cd8-scorecard/`. Add the history banner and fix relative links. Change nothing else.
2. Write the new latest.md with the same structure: environment, commands, summary, single-client latency, concurrency, sync and correctness. Generate the tables from the JSON with a script. Check every prose claim against the tables.
3. Add a "changes since `57a0cd8`" section that states measured deltas for range/scan, sync and anything else that moved noticeably. Attribute a delta to a specific change only when that change is the only plausible cause (for example, range pushdown for `range`/`ordered LIMIT`); otherwise state it without attribution.
4. Refresh the README table and prose from latest.md, including its date and the range/scan sentence (currently "about 90 ms at 50k rows").

## Acceptance criteria

- All four JSONs record revision `c1aa399…`, an empty `working_tree_status`, suite v1 and the default config; the external ones record `server_version`.
- The previous latest.md and its JSONs are archived unchanged apart from the banner and links.
- Every number in latest.md and the README traces to the JSONs (checked by script).
- Correctness failures are listed with their reasons. A regression in RepoDB correctness is filed as a bug and stops publication.
- Relative links resolve; `make lint` passes.
