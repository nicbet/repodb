# rdb-d690aa: Journal durability levels, default `normal`

## What / Why

See the issue for the measurements. One journal flush (`(*os.File).Sync`, which Go implements as `fcntl(F_FULLFSYNC)` on macOS) is 4.82 of 5.25 ms per single-row commit. RepoDB's own work is ~0.43 ms. Other databases don't do a full drive-cache flush per commit on macOS by default. Add three journal durability levels and default to **normal** (user decision, 2026-10-02).

## Levels

| level | macOS | Linux | Windows | an acknowledged journal commit survives |
|---|---|---|---|---|
| `full` | `fcntl(F_FULLFSYNC)` | `fdatasync` | `FlushFileBuffers` | power loss |
| `normal` (**default**) | `fcntl(F_BARRIERFSYNC)` | `fdatasync` | `FlushFileBuffers` | process crash and OS crash. On power loss, a suffix of the most recent commits may be lost; the journal never corrupts. |
| `off` | none per commit | none per commit | none per commit | process crash (the data is in the OS page cache). An OS crash or power loss may lose commits since the last flush. |

`F_BARRIERFSYNC` (fcntl 85) is Apple's documented cheaper alternative. It flushes to the device and orders writes before the barrier ahead of writes after it, but doesn't force the drive cache to stable media. If it returns `ENOTSUP` (for example on network filesystems), fall back to `F_FULLFSYNC`; that's Go's own fallback logic, inverted.

## Why weaker levels can't corrupt the journal

The journal is append-only, CRC-framed and length-prefixed (`readJournalFrames`). A crash can lose a **suffix** of it. Replay already treats a short or partial final frame as an incomplete tail and truncates it before the next append. `F_BARRIERFSYNC` keeps record order, so record N+1 never survives without record N; `fdatasync` and `FlushFileBuffers` are full flushes. With `off`, the OS page cache survives a process crash, and an OS crash leaves a prefix after the filesystem's own journal recovery. So every level loses at most the newest acknowledged commits, never a middle record and never consistency.

## Invariants that ignore the level

1. **A checkpoint flushes the journal fully before publishing.** `Checkpoint` and `CheckpointPrepared` call a full flush (`F_FULLFSYNC` / `fdatasync` / `FlushFileBuffers`) of the journal file before `CommitWithOutcomeMessage`. Without it, a power loss could leave Git holding a checkpoint commit that includes transactions the journal lost. Replay would then see a dirty journal whose base isn't the head: the stranded state rdb-e0c717 guards against. Checkpoints are rare, so the cost is negligible.
2. **The checkpoint marker and compaction stay fully flushed.** `compactLocked`'s `writeSynced` and `syncDir` keep a full flush, so the anchor never points at a Git commit the journal doesn't durably reflect.
3. **Git object and ref writes are unchanged** (`core.fsync=committed,reference`, `durabilityMethod`). Native-git persistence ("audit mode") isn't affected by this setting. Git batch flushing is a separate follow-up.

## Implementation

- **`common/repository`.**
  - A `Durability` type (`DurabilityFull`, `DurabilityNormal`, `DurabilityOff`; zero value = `DurabilityNormal`), with `ParseDurability(string)`.
  - `WorkingState` gets a durability field, set through `OpenWorkingStateWithOptions(repo, WorkingOptions{Durability})`. `OpenWorkingState` keeps the default.
  - A platform-specific `flushJournal(file, level)` in `flush_darwin.go`, `flush_linux.go` (and other unix) and `flush_windows.go`.
  - `groupSync` calls `flushJournal(file, w.durability)`; with `off` it skips the flush entirely.
  - The checkpoint pre-publish flush uses `DurabilityFull`.
  - A test seam (an unexported flush hook, set from `export_test.go`) records which flush ran, and in what order relative to publication.
- **`engine`.** `Options.Durability` is passed to the journal `WorkingState`, and ignored for native-git.
- **`server`.** `Config.Durability`.
- **CLI.** `repodb start --durability full|normal|off` and the same flag on `repodb-server` (default `normal`). The startup line reports it. Invalid values fail flag parsing.
- **dbbench.** A `-durability` flag (default `normal`). Reports record `"durability"`, and `docs/benchmark.md` states it. Results are only comparable at the same level.

## Tests

- **Flush selection, per level:** each journal commit calls the expected primitive (`full` → full flush, `normal` → barrier/fdatasync, `off` → none), checked through the hook.
- **Checkpoint ordering:** under `normal` and `off`, a checkpoint performs a full journal flush *before* the Git publication (hook records the order). Compaction keeps its full flushes.
- **Process crash, all levels:** a subprocess commits N rows in journal mode and is killed with SIGKILL without closing (reusing the `TestKilledWriterHelper` pattern). Reopening finds all N rows, including under `off`.
- **Parse and config:** `ParseDurability`; CLI and server flags and defaults; the engine passes the option through; native-git ignores it.
- **darwin:** an integration test calls the real `F_BARRIERFSYNC` path and checks it succeeds, including the `ENOTSUP` fallback through the hook.
- **Benchmark, recorded in the issue:** single-row journal write p50 at 50k rows on macOS for `full`, `normal` and `off`. Expected ~5.2 / ~0.65 / ~0.45 ms.

## Docs

- `docs/architecture.md`, journal durability section:
  - what each level guarantees, per OS;
  - why nothing corrupts (append-only, CRC framing, ordered or full flushes);
  - the checkpoint full-flush rule.
- `docs/cli.md`: `--durability` on `repodb start` / `repodb-server`; the backup and durability notes.
- `docs/library.md`: `engine.Options.Durability`, `server.Config.Durability`, and that `RecoverTransaction` reflects what reached disk, which under `normal` / `off` may exclude commits acknowledged right before a power loss or OS crash.
- `docs/benchmark.md`: the durability level used and why it matters for comparisons.
- README: a short line in the persistence description.
- `latest.md` isn't touched here; the next scorecard refresh (with rdb-d58a06) uses and states the level.

## Decisions

- **Default `normal`** (user). It matches what MySQL, PostgreSQL and SQLite do by default on macOS, and is a real flush on Linux and Windows.
- **Durability is per engine, not stored in the repository.** Engines on the same journal may use different levels; each controls only its own commits' flush. The checkpoint rule protects shared invariants.
- **`off` does no background flushing.** It relies on the OS's writeback, and its docs say so. A timed background flush (like PostgreSQL's `wal_writer_delay`) can come later if wanted.

## Out of scope

- Group commit (rdb-8f75fc): concurrent throughput.
- Git batch flushing for native-git and checkpoints (follow-up).
- Shrinking the 4.2 KB journal record per single-row update (follow-up).
