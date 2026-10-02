# Walkthrough: rdb-db5459, the append-heavy and mutable workload groups (dbbench suite v2)

## What was built and why

The scorecard (suite v1) measures database primitives on a two-column table with 32-byte values. That's useful for regressions, but it says nothing about how RepoDB behaves for the two shapes its consumers actually run:
- an **agent trace / event log**: append-heavy, time-ordered, read by recent window and by session;
- an **issue board**: a fixed set of rows, edited over and over, with hot rows, read by filters on indexed columns.

This issue adds those two shapes to `dbbench` as workload groups, `append` and `mutable`, runs them at 1k/10k/100k rows in all four modes, and records what the numbers say about where optimisation effort should go. The findings comment on the issue is the analysis. This walkthrough explains the harness.

## How the pieces fit

### One runner, three groups

`docs/benchmark.md` says to extend the one runner rather than add another benchmark. So `-workloads core,append,mutable` selects groups, and the default runs all three. `core` is the whole of v1, untouched, which is why its rows still compare with a v1 report. The suite version became 2 because a default run now contains different work.

The new groups have their own size flag, `-workload-rows`, defaulting to 1k/10k/100k (the user's choice). Changing the existing `-rows` default would have doubled the top size for the v1 workloads.

### `conn` and `backend`: written once, run everywhere

The v1 code exists twice, once in `main.go` against `engine.Session` and once in `external.go` against `database/sql`. The new groups avoid that duplication (`workloads.go`).

- **`conn`** is one client session: `exec`, `query` (rows as strings), `tx(fn)` and `close`. A rejected optimistic-concurrency write comes back as an error wrapping `repository.ErrConflict`, after the session has been made reusable.
  - On RepoDB that means issuing `ROLLBACK` after a conflicting autocommit statement or `COMMIT` (`rdbConn.rollbackConflict`), the same thing v1's concurrency code does inline.
  - On MySQL and Dolt, errors 1213 (a deadlock; Dolt also uses it for serialization failures) and 1205 (lock wait timeout) are mapped to the same error (`externalConflict`), so `measure()` counts them as conflicts.
- **`backend`** is the persistence-specific part: setting up the fixture, opening connections, and, on RepoDB only, sync, housekeeping, journal and fixture sizes, and peer cloning.
  - `repoBackend` reuses v1's `database` adapter, so sync includes the checkpoint, as everywhere else in the scorecard.
  - `externalBackend` reports `syncs() == false`, and the group code skips everything Git-specific based on that.

`runWorkloadGroups` loops over groups and sizes, and asks the caller for a fresh backend each time. `run()` passes RepoDB backends and `runExternal()` passes external ones; nothing else differs between modes.

`env` carries the report, group, size and backend for one run. `env.measure` delegates to the existing `report.measure` and stamps the measurement with `group`. Setup steps that both groups share go through `env.setup`, which prefixes the name (`append_bulk_load`, `mutable_bulk_load`). Workload names are already unique, so they stay as the spec named them.

### Fixtures

Every row is a pure function of its id (`makeEvent`, `makeIssue`, `commentBody`), so a run is reproducible and a test can check the shape.

- Text is pseudo-prose drawn from a fixed vocabulary (`filler`), not hex. Git and zlib compress real text several times better than random characters, and the repository-growth numbers would mean nothing otherwise.
- Event timestamps step by 10 ms with under 10 ms of jitter, so `ts` strictly increases with `id`. That lets `events_recent` check the exact newest id.
- Sessions are contiguous runs of 100 events, as a real trace's are, so `events_session` returns exactly 100 rows.
- Hot rows: `hotPicker` draws Zipf(s = 1.1) ranks and maps rank 0 to the **newest** issue, because recently filed issues are the hot ones on a board.
  - A side effect is that hot writes cluster at the right edge of the trees, which is realistic, but it is not the worst case for scattered tree mutation.
  - The skew is stronger than the spec's "about 80/20": 84% of draws land on the newest fifth at 1k and 93% at 100k. `docs/benchmark.md` states the measured figures rather than the guess.

### Growth phase and `series`

`growth()` runs `-growth-rounds` rounds of a fixed burst, then a sync:
- append: ten 100-event transactions, each followed by one rare update;
- mutable: 500 hot status updates and 100 comments.

Each round adds a `growthPoint` to `report.Series`:
- write p50/p95/max for the burst;
- sync time;
- absolute fixture bytes (local plus remote) and the change since the previous round;
- journal directory bytes before and after the sync;
- process heap.

Fields a backend can't measure are pointers left nil, so external rows omit them. Git housekeeping runs once before the phase and **not between rounds**, on purpose: the series shows unmaintained growth, which is the cost that compaction and retention work would remove.

After the rounds, a peer is cloned, enabled and opened once, and that step is timed as `<group>_peer_pull_after_growth`. The local and peer databases must then return identical results for a few checksum queries. A mismatch is recorded as the measurement's `VerificationError`, so it prints as FAIL under v1's rules.

`event_update_rare` was a judgement call. The spec wanted one update per 100 appends, interleaved. A 30-request workload never reaches a 100th append, so the measured workload is plain autocommit payload rewrites, and the 1-in-100 ratio lives in the growth bursts, where it actually happens.

### Reporting

`printReport` prints one table per group. `core` rows carry no `group`, so v1 report consumers see nothing new for them. `printSeries` follows, showing the first, middle and last round for each group and size; JSON keeps every round.

### Profiling hooks

These were added after the reference runs to back the findings with evidence, and they don't change any workload.
- `measure()` starts each worker goroutine under `pprof.Do` with `workload` and `clients` labels. The growth burst and growth sync get `<group>_growth_burst` and `<group>_growth_sync`. So `-cpuprofile` produces one profile for the whole run, and `go tool pprof -tagfocus <workload>` slices it.
  - Gotcha: `-tagfocus clients=16` matched nothing. pprof appears to treat numeric-looking label values differently. Focusing on the workload name worked.
- `-pprof <addr>` serves `net/http/pprof` with mutex and block profiling enabled, for heap captures while a run is in progress.
- The CPU profile is stopped explicitly before the report is written, because `main` exits through `os.Exit`, which skips deferred calls.

## Decisions and their rationale

- **Reference runs used a dirty tree** (`BENCH_ALLOW_DIRTY=1`, from the worktree: main plus the harness only). xpo commits at merge time, and these results go only into the issue, not into `latest.md` (the user's decision). The reports record the dirty status.
- **Don't touch the worktree while `make bench-docker` runs.** Each mode rebuilds the image from the working tree. Editing code between modes would have measured different binaries under one revision. The profiling flags were therefore added only after all four runs finished.
- **The bugs the profiles exposed were filed, not fixed.** rdb-187370 (concurrent sessions re-validate the whole database) and rdb-93103f (O(database) snapshot loads) are engine changes with real design choices, outside a measurement issue, so they went to the backlog with their evidence.

## Anything non-obvious for a future reader

- `-workload-rows` has a minimum of 1000. Below that, `issue_board_query`'s exact `LIMIT 50` and the per-session counts are no longer guaranteed.
- The quick harness check in `docs/benchmark.md` now passes `-workload-rows 1000 -growth-rounds 2`. Otherwise a "quick" run executes the new groups at 100k.
- `make bench-docker` empties the shared fixture volume before **every** mode, including external runs. To inspect a RepoDB fixture after a full four-mode run, use a separate volume, as the profiling runs did with `repodb-bench-prof`.
- The external backend's `root()` is the harness's empty fixture root, so file growth for external measurements is always 0. Server-side storage is not measured.
