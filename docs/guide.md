# RepoDB User Guide

This guide walks through the full RepoDB lifecycle — from setup through synchronization. It complements the [README quickstart](../README.md) and links to the detailed subsystem docs where appropriate.

## Setup

RepoDB requires an existing Git repository. From inside one, initialize the data namespace:

```sh
repodb init
```

This creates an initial data commit on the ref `refs/repodb/data`. It never touches source branches, the user's index, or the working tree. An optional path argument initializes a different directory:

```sh
repodb init /path/to/repo
```

If the repository already has RepoDB data, `init` returns an error. Use `repodb enable` instead when adopting data from a remote (see
[Synchronization](#synchronization)).

## Starting the server

RepoDB includes a standalone MySQL-compatible server:

```sh
repodb start
```

**Server flags**

| Flag            | Default          | Description                                 |
| --------------- | ---------------- | ------------------------------------------- |
| `--addr`        | `127.0.0.1:3306` | Listen address and port                     |
| `--repo`        | `.`              | Path inside the Git worktree                |
| `--persistence` | `native-git`     | Persistence mode: `native-git` or `journal` |

The server listens on `127.0.0.1:3306` by default. Any MySQL client connects with no password:

```sh
mysql --host=127.0.0.1 --port=3306 --user=root repodb
```

## Persistence modes

RepoDB offers two persistence modes. Both provide immediate SQL durability — they differ in how transactions reach Git history.

### 1. Native Git (default for server)

Every SQL commit creates a Git data commit. Simple and complete: every transaction appears in `git log refs/repodb/data`. Slower per transaction
because each commit writes objects, updates trees, and advances the ref.

### 2. Journal (default for embedded, opt-in for server)

SQL commits append to a local journal file (`<git-common-dir>/repodb/working/v1/journal`) with a single `fsync`. Much faster (~5 ms per transaction regardless of batch size), but changes are not in Git history until checkpointed.

### Switching modes

**Server:**

```sh
repodb start --persistence journal
repodb start --persistence native-git
```

**Embedded Go API:**

```go
eng, err := engine.Open(ctx, ".", engine.Options{
    Persistence: engine.PersistenceJournal,
})
```

### Checkpointing

In journal mode, use `repodb commit` to materialize accumulated journal changes into a Git data commit:

```sh
repodb commit -m "checkpoint after data migration"
```

Until a checkpoint, data is durable locally (in the journal) but not visible in Git history and not syncable. `repodb sync` refuses to run if the journal is dirty — checkpoint first.

### Mixed-mode guard

The native-git engine refuses to open while journal state is dirty. This prevents two persistence modes from writing concurrently. Checkpoint or discard the journal before switching back to native-git.

## Data lifecycle

### Status

Show the current data head, format version, object and table counts, and working generation:

```sh
repodb status
```

Output includes whether the working state is `dirty` (journal has uncheckpointed changes) or `clean`.

### Diff

Show uncommitted data changes at the table level:

```sh
repodb diff
```

Lists tables with pending inserts, updates, or deletes since the last
checkpoint.

### Commit

Create a data commit from the current working state:

```sh
repodb commit -m "describe the change"
```

The `-m` flag is required. This checkpoints the journal (in journal mode) or is a no-op confirmation (in native-git mode where each SQL commit is already a Git commit).

| Flag     | Default    | Description                  |
| -------- | ---------- | ---------------------------- |
| `-m`     | (required) | Data commit message          |
| `--repo` | `.`        | Path inside the Git worktree |

## CLI reference

| Command                       | Description                                                            |
| ----------------------------- | ---------------------------------------------------------------------- |
| `repodb init [path]`          | Initialize a RepoDB data namespace in a Git repository                 |
| `repodb start`                | Start the MySQL-compatible server (`--addr`, `--persistence`)          |
| `repodb sql '<statement>'`    | Execute SQL against a running server                                   |
| `repodb status`               | Show data head, format version, and working state                      |
| `repodb diff`                 | Show uncommitted data changes                                          |
| `repodb commit -m '<msg>'`    | Checkpoint working data into a Git data commit                         |
| `repodb enable`               | Set up sync for a remote (`--remote`)                                  |
| `repodb sync`                 | Fetch and publish data history, merging independent edits (`--remote`) |
| `repodb conflicts`            | List unresolved merge conflicts after a sync (`--remote`)              |
| `repodb resolve`              | Resolve a specific merge conflict (`--id`, `--take`, `--remote`)       |
| `repodb import-legacy [path]` | Import data from the legacy `.repodb` format                           |

### `repodb sql`

Execute a SQL statement against a running RepoDB server:

```sh
repodb sql 'SELECT * FROM events WHERE id > 10'
```

| Flag         | Default          | Description    |
| ------------ | ---------------- | -------------- |
| `--addr`     | `127.0.0.1:3306` | Server address |
| `--database` | `repodb`         | Database name  |
| `--user`     | `root`           | MySQL user     |
| `--password` | (empty)          | MySQL password |

### `repodb enable`

Set up synchronization for a remote:

```sh
repodb enable --remote origin
```

This adds a fetch refspec (`+refs/repodb/data:refs/repodb/remotes/origin/data`) and writes `repodb.remote=origin` to Git config. If the remote has data and the local side does not, enable fetches and adopts. If neither side has data, it initializes an empty catalog. Safe to run repeatedly.

| Flag       | Default | Description                  |
| ---------- | ------- | ---------------------------- |
| `--remote` | (none)  | Git remote name              |
| `--repo`   | `.`     | Path inside the Git worktree |

### `repodb sync`

Exchange committed data history with a remote:

```sh
repodb sync --remote origin
```

The sync cycle: fetch → validate → lock → compare ancestry → fast-forward or three-way merge → push. Never force-pushes. When both sides have diverged, sync performs a three-way merge and creates a two-parent commit.

If merge conflicts arise, sync completes the fetch and reports the conflicts. Use `repodb conflicts` and `repodb resolve` to handle them (see
[Conflict resolution](#conflict-resolution)).

| Flag       | Default | Description                  |
| ---------- | ------- | ---------------------------- |
| `--remote` | (none)  | Git remote name              |
| `--repo`   | `.`     | Path inside the Git worktree |

### `repodb conflicts`

List merge conflicts after a sync:

```sh
repodb conflicts --remote origin
```

Shows the base, local, and remote values for each conflicting row, along with resolution status. Conflict state is stored at
`<git-common-dir>/repodb/conflicts/<remote>.json` and survives restarts.

| Flag       | Default | Description                  |
| ---------- | ------- | ---------------------------- |
| `--remote` | (none)  | Git remote name              |
| `--repo`   | `.`     | Path inside the Git worktree |

### `repodb resolve`

Resolve a specific conflict:

```sh
repodb resolve --remote origin --id '<conflict-id>' --take local
```

Resolution choices:

| Choice   | Effect                        |
| -------- | ----------------------------- |
| `local`  | Keep the local value          |
| `remote` | Accept the remote value       |
| `base`   | Revert to the common ancestor |
| `delete` | Remove the row                |

When all conflicts are resolved, the merge commit is published automatically.

| Flag       | Default    | Description                         |
| ---------- | ---------- | ----------------------------------- |
| `--remote` | (none)     | Git remote name                     |
| `--repo`   | `.`        | Path inside the Git worktree        |
| `--id`     | (required) | Conflict ID from `repodb conflicts` |
| `--take`   | (required) | Resolution choice                   |

### `repodb import-legacy`

Import data from the legacy `.repodb` storage format:

```sh
repodb import-legacy
```

Verifies the old store, publishes its data as an initial commit, and retains
the `.repodb/` directory for review.

## Synchronization

RepoDB data travels separately from source code. Ordinary `git push` and `git pull` move source branches but do not publish or reconcile RepoDB data.

### Workflow

1. **Enable** — run once per clone to set up tracking:

   ```sh
   repodb enable --remote origin
   ```

2. **Work** — create tables, insert and update data via the server or embedded API.

3. **Checkpoint** — if using journal mode, commit before syncing:

   ```sh
   repodb commit -m "ready to share"
   ```

4. **Sync** — exchange data with the remote:

   ```sh
   repodb sync --remote origin
   ```

5. **Resolve** — if conflicts arise, inspect and resolve them:
   ```sh
   repodb conflicts --remote origin
   repodb resolve --remote origin --id '<id>' --take local
   ```

### What Git commands do to RepoDB data

| Git command | Source refs          | RepoDB data                              |
| ----------- | -------------------- | ---------------------------------------- |
| `git clone` | Normal               | Not fetched until `enable`               |
| `git fetch` | Normal               | Updates tracking ref only                |
| `git pull`  | Fetch + merge/rebase | Fetches tracking data, no reconciliation |
| `git push`  | Normal push rules    | Does not push data                       |

No hooks are installed. There is no atomicity between source and data histories. `repodb sync` is the only guaranteed network operation for data.

## Conflict resolution

Sync performs a three-way merge using the Git merge-base of the local and remote data commits. Rows are compared by encoded primary key.

**Auto-accepted** (no conflict): identical results on both sides, one-side-only changes, or changes to different keys.

**Conflicts** arise from: competing updates to the same row, insert/insert with different values for the same key, or update/delete on the same row. Schema conflicts occur when a table is created differently on both sides or when one side modifies a table the other deletes.

Rows are the smallest merge unit — there is no field-level merge.

### Distributed identity

When multiple clones insert rows independently, auto-increment keys can
produce collisions during sync. Prefer UUIDs or composite keys that include
writer identity for tables shared across clones. Auto-increment is fine for
single-clone or single-writer workflows. See the [merge guide](merge-m4.md)
for full details.

## Embedded Go API

For Go applications that use RepoDB as a library:

```go
db, err := engine.Open(ctx, ".")
defer db.Close()

session, _ := db.NewSession()
defer session.Close()

session.Exec(ctx, "CREATE TABLE events (id BIGINT PRIMARY KEY, data TEXT NOT NULL)")
session.Exec(ctx, "INSERT INTO events VALUES (?, ?)", 1, payload)

result, _ := session.Query(ctx, "SELECT data FROM events WHERE id = ?", 1)
```

Each concurrent unit of work needs its own session. Sessions use snapshot isolation: reads see a pinned snapshot plus the session's own writes. A stale writer receives `ErrConflict` rather than silently overwriting newer data.

DDL statements (`CREATE TABLE`, `DROP TABLE`, `ALTER TABLE`) trigger an implicit commit. A failed DML statement rolls back that statement's changes but keeps the transaction active.

To recover from an unknown-outcome commit (e.g. process crash during commit), use `engine.RecoverCommit` rather than replaying SQL.

See [SQL and embedded API](sql-m2.md) for supported types, transaction semantics, and workload bounds.

## Backup and recovery

### What to back up

- **Git repository** — contains all checkpointed data history.
- **Journal** (`<git-common-dir>/repodb/working/v1/journal`) — contains uncheckpointed SQL commits. Without this, recovery rolls back to the last checkpoint.

Both must be copied at the same coordinated point for a consistent backup of uncommitted data. Copying only the Git repository recovers the last
checkpointed commit.

### Cache

The cache directory (`<git-common-dir>/repodb/cache/`) is disposable and safe to delete. It is rebuilt on next access.

### Worktrees

Linked Git worktrees share the journal and publication lock with the main worktree. There is one journal per repository, not per worktree.

### Legacy import

If the repository has data in the old `.repodb/` format, import it:

```sh
repodb import-legacy
```

The old files are retained for review after import.

## Git interaction

RepoDB stores data on `refs/repodb/data`, a ref outside the normal branch namespace. It uses a temporary index under `<git-common-dir>/repodb/tmp` and never touches the user's index or working tree.

Publication is serialized by a POSIX file lock at `<git-common-dir>/repodb/locks/publish.lock`. The lock is shared across
worktrees.

Git commands that operate on source branches (`checkout`, `merge`, `rebase`) have no effect on RepoDB data. Concurrent fetch or sync operations do not affect in-flight SQL transactions because sessions pin immutable snapshots.

RepoDB performs a full integrity check on open: format version, manifest, object inventory, tree membership, and SHA-256 content verification. Both SHA-1 and SHA-256 Git repositories are supported.

See [storage format](storage-format.md) for the full ref layout, object encoding, and durability guarantees.
