# rdb-c56142 walkthrough: journal is the default everywhere

## Why

Journal persistence writes in ~5 ms where native-git needs a Git object, tree, commit and ref update per transaction. The library already defaulted to journal, but `repodb start` and `repodb-server` didn't. Two blockers had to go first:
- unbounded journal growth: fixed by rdb-4d677f, compaction at checkpoint;
- head moves under a dirty journal: fixed by rdb-e0c717, the publication guard.

## Defaults

`--persistence` defaults to `journal` in both binaries. Native-git is presented as **audit mode**: every transaction is a Git data commit, the Git repository alone is a backup, and there's no checkpoint step. Flag parsing moved into `parseStartOptions` and `parseOptions` so tests can check the defaults, and the startup line names the mode.

## The real problem: syncing while a server writes

With journal as the default, `repodb sync` has to turn journal changes into a data commit first. Doing "checkpoint, then sync" as two steps loses the race whenever a server commits during sync's fetch and validation, which takes seconds on large tables. Since rdb-e0c717, the publication correctly refuses to move the head under that new journal transaction.

So the checkpoint became part of sync's retry loop (`integration.SyncWithOptions` with `SyncOptions.Checkpoint`):
- each attempt starts with `cleanWorkingState`, which checkpoints if the journal is dirty;
- a journal commit seen after the fetch, or refused at publication (fast-forward or merge commit), sets `working-dirty-retry` and continues. The next attempt checkpoints the newcomer and re-reads the local head, so merges are recomputed against it;
- after 3 attempts, the existing "did not stabilize" error wraps `ErrWorkingDirty`.

`Sync` is `SyncWithOptions` with no options, unchanged: it fails on a dirty journal.

The CLI exposes this as `repodb sync --commit -m <message>`. The name matches `repodb commit -m`, which is what the CLI calls a checkpoint. The terminal prompt reuses the same path. Without a terminal or the flag, sync fails with a message naming `--commit -m`, so scripts know what to run.

## Backup and switching modes

- **Backup.** No new command (online backup is rdb-f33cb0). The docs lead with journal mode: commit and then back up Git, or stop RepoDB and copy the whole repository. A test copies a stopped repository and finds its uncheckpointed rows.
- **Switching modes.** An existing native-git repository opens in journal mode as-is, because a missing journal means clean. Going back requires a checkpoint, because of the native-git open guard. A test covers both directions.

## Verification

Unit and integration tests cover:
- the defaults;
- the sync flag and the refusal without it;
- the retry under a racing journal commit (via the `beforePublish` hook from rdb-e0c717) and the give-up case;
- mode switching and the backup copy.

An end-to-end run with the built binaries confirmed that `sync --commit -m` pushes while a default (journal) server keeps running.
