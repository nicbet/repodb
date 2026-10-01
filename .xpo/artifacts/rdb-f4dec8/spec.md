# rdb-f4dec8: Retire milestone experiments, unused code and milestone wording

Agents and readers should find no milestone-named entry points or messages that suggest current workflows. The keep/remove list was approved by the user on 2026-10-01.

## Removed

- `experiments/gitstorage` and `make m0`: the Git-integration milestone prototype. It used only the standard library; `common/repository` and `integration` tests cover the behavior it checked.
- `experiments/m2bench` and `make m2-bench`: superseded by `dbbench` and `BenchmarkSQL*`.
- `common/storage` `Filesystem` and its test. The `Store` interface and the in-memory store stay. `common/storage` now has no tests of its own; the in-memory store is exercised through the Prolly and engine tests.
- `common/query` (package and smoke test). The server parses through go-mysql-server.
- `Repository.CacheDir()` and its test assertions. Nothing wrote to the cache directory. `docs/cli.md` drops the `repodb/cache/` line.
- **Added during implementation:** `Writer.PendingHashes()`, which had no callers and whose comment named "the journal prototype".

## Renamed

- `make m4.2-bench` → `make bench-go` (`BenchmarkSQL*` and the sync benchmarks).
- `make m4.3-bench` → `make bench-journal`.
- Benchmarks:
  - `BenchmarkM43DurableSave` → **`BenchmarkDurableSave`**. It runs both native-git and journal, so a `Journal` prefix would mislead. This deviates from the first spec, which proposed `BenchmarkJournalDurableSave`.
  - `BenchmarkM43JournalCheckpoint` → `BenchmarkJournalCheckpoint`.
  - `…JournalReplayGrowth` → `BenchmarkJournalReplayGrowth`.
  - `…JournalFirstSaveAfterCheckpoint` → `BenchmarkJournalFirstSaveAfterCheckpoint`.
  - `BenchmarkM43TypedEditJournal` → `BenchmarkJournalTypedEdit`.
  - `BenchmarkM43TypedEditCheckpoint` → `BenchmarkJournalTypedEditCheckpoint`.
- The `.PHONY` list was updated, both targets have a one-line Makefile comment, and `docs/testing.md` documents them.

## Reworded

| Old | New |
|---|---|
| `RepoDB M2 requires an explicit PRIMARY KEY` | `RepoDB requires an explicit PRIMARY KEY` |
| `unsupported M2 SQL type X` | `unsupported SQL type X` |
| `column c uses unsupported M2 schema behavior` | `column c uses AUTO_INCREMENT or a generated column, which RepoDB does not support` |
| `RepoDB M2 does not support savepoints` (3 sites) | `RepoDB does not support savepoints` |

Comments now describe current behavior: the `WorkingState` doc, `CommitTypedEdits` and the `Checkpoint` comment. The `"M4.3 benchmark checkpoint"` message is now `"benchmark checkpoint"`. `docs/sql.md` quotes updated; no test asserted the old strings.

## Not touched

`docs/benchmarks/latest.md`, and history in xpo artifacts.

## Acceptance

- `git grep` finds no milestone names, old targets, "prototype", `common/query` or `CacheDir` in Go code, the Makefile or `docs/` (except `latest.md`). The only hit is "Apple M1 Max", a CPU name in the README.
- `make build`, `make test` and `make lint` pass, and each renamed benchmark runs one iteration.
