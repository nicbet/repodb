# rdb-c56142: Journal is the default persistence for the CLI and server

## What

`engine.Open` and `server.Config` already defaulted to journal; `repodb start` and `repodb-server` defaulted to native-git. Make journal the default everywhere, and present native-git as an explicit **audit mode**: every transaction is a Git data commit, the Git repository alone is a complete backup, and there is no checkpoint step. Both dependencies are done: rdb-4d677f (bounded journal) and rdb-e0c717 (no head moves under a dirty journal).

## How

### 1. Defaults

`repodb start --persistence` and `repodb-server --persistence` default to `journal`; `native-git` stays selectable. Flag help says "journal (default) or native-git (audit mode: every transaction is a Git commit)", and both servers print the persistence mode at startup. Flag parsing moved into `parseStartOptions` (`cmd/repodb`) and `parseOptions` (`cmd/repodb-server`) so the defaults are tested.

### 2. Sync that checkpoints, including while a server writes

Previously `repodb sync` offered to checkpoint only on a terminal; otherwise it failed with `ErrWorkingDirty`. With journal as the default and a server writing, a separate "checkpoint, then sync" loses the race whenever a transaction commits during sync's fetch and validation (seconds at 50k rows), because rdb-e0c717 correctly refuses to move the head under a dirty journal.
- **Library.** `integration.SyncWithOptions(ctx, start, remote, SyncOptions{Checkpoint func(context.Context) error})`; `Sync` is `SyncWithOptions` with no options. With `Checkpoint` set, each of sync's (up to 3) attempts:
  - checkpoints first if the journal is dirty (`cleanWorkingState`);
  - treats a journal commit that arrives during the fetch, or that wins the race at publication (fast-forward or merge), as **retryable** (`working-dirty-retry`) instead of failing. The next attempt checkpoints again and re-reads the local head.

  Retry exhaustion returns the existing "did not stabilize after 3 attempts" error wrapping `ErrWorkingDirty`. Without `Checkpoint`, behavior is unchanged.
- **CLI.** `repodb sync --commit -m <message>` checkpoints with that message (printing each data commit it makes) and syncs, using the above.
  - `--commit` without `-m`, and `-m` without `--commit`, are errors.
  - Without the flag, on a terminal, the prompt asks once; a "yes" runs the same `--commit` path with the message `checkpoint before sync`.
  - Without the flag and without a terminal, it fails. The `ErrWorkingDirty` message now names `repodb sync --commit -m <message>`.

  The flag is called `--commit` to match `repodb commit -m`, the user-facing name for a checkpoint.

### 3. Backup guidance

There's no new command; online backup is rdb-f33cb0. `docs/cli.md` "Backup and recovery" leads with journal mode:
1. `repodb commit -m …`, then back up Git; or
2. stop RepoDB and copy the whole repository, `.git` included.

Native-git is listed second, as audit mode.

### 4. Mode switching

- An existing native-git repository opens in journal mode with no migration: no journal means clean.
- Switching back to native-git requires a checkpoint (the existing `ErrWorkingStateDirty` open guard).
- Both directions are tested and documented.

### 5. Docs

- README: sync paragraph, commands table, architecture bullet and library sentence.
- `docs/cli.md`: persistence-mode table and text, `repodb start` flag default and example, `repodb sync` (behavior and flags), backup.
- `docs/library.md`: server defaults, the `Sync` errors list, and `SyncWithOptions` with an example.
- `docs/architecture.md`: sync steps (checkpoint option, retry).
- `docs/testing.md`: new coverage, and the CLI gap note narrowed.
- `make bench` still defaults to native-git: that's the benchmark harness's mode selector, not the product default. `docs/benchmarks/latest.md` waits for the next scorecard.

## Decisions (confirmed by the user, 2026-10-01)

1. Flag name `--commit -m`, matching `repodb commit`.
2. Checkpoint inside sync's retry loop, so sync succeeds while a journal-mode server keeps writing. A busy writer still exhausts 3 attempts, and the error says so.
3. No backup command here; rdb-f33cb0 stays the place for online backup.

## Tests (as built)

- **`cmd/repodb`:**
  - `TestStartPersistenceDefaultsToJournal`;
  - `TestSyncCommitFlagCheckpointsAndSyncs`: a dirty journal becomes a commit with the given subject, pushed to the bare remote;
  - `TestSyncWithoutCommitFlagRefusesDirtyJournal`: no terminal, the error names `--commit -m`, plus both flag-misuse errors.
- **`cmd/repodb-server`:** `TestPersistenceDefaultsToJournal`.
- **`integration`:**
  - `TestSyncWithCheckpointRetriesRacingJournalCommit` (fast-forward and merge): a racing commit through the `beforePublish` hook is checkpointed and merged on the retry, and the journal ends clean;
  - `TestSyncWithCheckpointGivesUpOnConstantJournalCommits`;
  - the existing no-option tests are unchanged.
- **`engine/persistence_modes_test.go`:**
  - `TestSwitchingPersistenceModes`: native-git → journal → refused native-git open → checkpoint → native-git;
  - `TestCopiedRepositoryKeepsUncheckpointedRows`.
- **Manual end-to-end** with the built binary: `repodb start` (no flags) reports journal persistence; SQL writes; plain `repodb sync` without a terminal refuses with the new message; `repodb sync --commit -m "from e2e"` pushes while the server runs; the remote shows the commit.

## Acceptance

As in the issue (the server and CLI default to journal; non-interactive sync with and without the flag; backup tested), plus the tests above. `make test` and `make lint` pass. The issue's "next scorecard reflects the default" waits for the next scorecard refresh.
