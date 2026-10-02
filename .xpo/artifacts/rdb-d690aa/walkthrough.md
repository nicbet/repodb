# rdb-d690aa walkthrough: journal durability levels, default `normal`

## Why

The scorecard showed journal single-row writes at ~5 ms against 0.7–1.1 ms for MySQL and 0.9–1.5 ms for Dolt. A breakdown found 4.8 of 5.25 ms in one call: `(*os.File).Sync`, which Go implements on macOS as `fcntl(F_FULLFSYNC)`, a forced flush of the SSD's cache. RepoDB's own work was ~0.43 ms. Other databases don't do that by default on macOS (MySQL plain `fsync`, SQLite `fullfsync=off`, PostgreSQL only with `fsync_writethrough`). The scorecard's Docker baselines don't reach the Mac's SSD at all: 0.16 ms per synchronous write in the VM. The user chose to add levels and default to `normal`.

## The levels (`common/repository/durability.go`, `flush_*.go`)

| level | macOS | Linux | Windows |
|---|---|---|---|
| `full` | `F_FULLFSYNC` (`File.Sync`) | `fdatasync` | `FlushFileBuffers` |
| `normal` (default) | `F_BARRIERFSYNC` (fcntl 85), falling back to full on `ENOTSUP`/`EINVAL` | `fdatasync` | `FlushFileBuffers` |
| `off` | no per-commit flush | same | same |

`Durability` is a string type whose zero value means `normal`, so existing callers and zero-valued options get the new default. `flushFile` is a package variable so tests can record flushes; production code never replaces it.

## Why weaker levels are safe for this journal

The journal is append-only, and every record is length-prefixed and CRC-checked. Replay already truncates a partial final record. Barrier and full flushes keep records in order, and `off` relies on the OS, which keeps the page cache in order for a single appended file and recovers a prefix after an OS crash. So any crash loses at most the newest commits, never a middle record.

## The rule that ignores the level

A checkpoint publishes a Git commit containing the journal's transactions. If a power loss kept that commit but not the journal records it came from, the journal would replay as dirty on a base that isn't the head: the stranded state of rdb-e0c717. So `Checkpoint` and `CheckpointPrepared` call `flushJournalFully()` before `CommitWithOutcomeMessage`, and compaction's anchor file is always fully flushed. Checkpoints are rare, so this costs little.

## Wiring

- `WorkingState.durability` is set by `OpenWorkingStateWithOptions`; `groupSync` flushes at that level and returns at once for `off`.
- `engine.Options.Durability`, `server.Config.Durability`, `--durability` on both CLIs (parsed with `ParseDurability`, printed at startup) and dbbench `-durability`, whose reports record `durability` (also passed to the reopen subprocess).
- `repodb-server` now prints flag errors instead of exiting silently.
- Native-git commits are Git commits; the setting doesn't affect them.

## Tests

- **Per-level flushes:** `off` makes none, and the zero value means normal.
- **Checkpoint order:** the first flush during a checkpoint is full, and runs while the head is still the old one. Removing the pre-publish flush makes the test fail.
- **Real flushes:** each level's actual flush call succeeds on the platform.
- **Process kill:** a subprocess commits 20 rows and is killed with SIGKILL; all rows survive at every level.
- **Configuration:** the CLI flags for both binaries and the engine option pass-through.

## Result

macOS, single-row autocommit `UPDATE` at 50k rows, p50: `full` 5.01 ms, **`normal` 0.47 ms**, `off` 0.21 ms.

## Not covered

- The `F_BARRIERFSYNC` → full fallback isn't exercised; APFS supports barrier fsync.
- Group commit (rdb-8f75fc) and Git batch flushing for native-git remain separate.
- A single-row journal record is ~4.2 KB, which is worth trimming later.
