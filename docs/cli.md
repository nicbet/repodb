# Command-line usage

RepoDB ships two programs:

- `repodb`: initializes a repository, runs the MySQL-compatible server, checkpoints journal changes, and synchronizes data with a Git remote.
- `repodb-server`: a server-only binary with the same flags as `repodb start`.

Both work on the Git repository that contains the current directory, or the one named by `--repo` where a command accepts it. RepoDB data lives on its own ref, `refs/repodb/data`. It never changes your branches, index or working tree. See [architecture.md](architecture.md) for how the data is stored.

On failure, `repodb` prints `repodb: <error>` to standard error and exits with status 1.

## Install

Build from a checkout with Go 1.26 or newer, Git and Make:

```sh
make build                      # writes bin/repodb and bin/repodb-server
export PATH="$PWD/bin:$PATH"
```

Or install the programs directly:

```sh
go install -tags gms_pure_go github.com/nicbet/repodb/cmd/repodb@latest
go install -tags gms_pure_go github.com/nicbet/repodb/cmd/repodb-server@latest
```

The `gms_pure_go` tag selects go-mysql-server's pure-Go regular-expression backend. Without it you need cgo and ICU4C. See [library.md](library.md#build-requirements).

## Persistence modes

Every SQL write is durable once it returns. The two modes differ in when a write becomes a Git commit:

| | `native-git` | `journal` |
| --- | --- | --- |
| A committed SQL transaction… | publishes a Git data commit immediately | appends to a local journal with one `fsync` |
| Git history | one data commit per transaction | one data commit per checkpoint (`repodb commit`) |
| Write latency | Git object, tree, commit and ref work per transaction (see [benchmarks/latest.md](benchmarks/latest.md)) | one journal append and `fsync` |
| Before `repodb sync` | nothing to do | checkpoint the journal first |
| Backup | the Git repository | the Git repository **and** the journal, copied at one point |
| Default | no: opt in with `--persistence native-git` | yes: `repodb start`, `repodb-server` and the Go library (`engine.Open`) |

Journal is the default and much faster per write. Native-git is **audit mode**: every transaction appears in `git log refs/repodb/data`, the Git repository alone is a complete backup, and there is no checkpoint step.

The modes share one repository, and switching needs no migration:
- **native-git → journal:** open in journal mode; a repository without a journal is clean.
- **journal → native-git:** a native-git engine refuses to open while the journal has uncheckpointed changes, so run `repodb commit` first.

## Setting up a repository

### `repodb init [path]`

Creates the first, empty data commit on `refs/repodb/data` in the Git repository at `path` (default `.`), and prints the ref and commit:

```sh
git init my-project && cd my-project
repodb init
```

It fails if the repository already has RepoDB data. On a clone of a project whose remote already has RepoDB data, run `repodb enable` instead: `init` would start an unrelated history.

## Running the server

### `repodb start`

Starts the MySQL-compatible server and runs until interrupted (Ctrl-C or `SIGTERM`).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `127.0.0.1:3306` | listen address |
| `--repo` | `.` | a path inside the Git repository |
| `--persistence` | `journal` | `journal`, or `native-git` for audit mode |

```sh
repodb start
mysql --host=127.0.0.1 --port=3306 --user=root repodb
```

The database is named `repodb`. The server performs no authentication and has no TLS. Keep it on a loopback address; authentication, TLS and privileges are tracked in rdb-d0b39d, rdb-5720c1 and rdb-8d63a3. The SQL the server accepts is described in [sql.md](sql.md).

### `repodb-server`

Takes the same `--addr`, `--repo` and `--persistence` flags. The server exposes one database, named by the repository's manifest (`repodb`).

### `repodb sql <statement>`

Runs one statement against a running server and prints the result as tab-separated columns with a header row, with `NULL` for SQL NULL. All remaining arguments are joined with spaces into one statement.

| Flag | Default |
| --- | --- |
| `--addr` | `127.0.0.1:3306` |
| `--database` | `repodb` |
| `--user` | `root` |
| `--password` | empty |

```sh
repodb sql 'CREATE TABLE notes (id BIGINT PRIMARY KEY, body TEXT NOT NULL)'
repodb sql "INSERT INTO notes VALUES (1, 'hello')"
repodb sql 'SELECT * FROM notes'
```

## Inspecting and checkpointing local changes

These commands operate on the repository containing the current directory. `status` and `diff` take no flags.

### `repodb status`

Prints the data head commit, the storage format version, and the number of objects and tables in the committed snapshot. If a journal exists, it also prints the working generation and whether it is `dirty` (it has changes not yet checkpointed) or `clean`.

### `repodb diff`

Lists tables that differ between the journal's working state and the last checkpoint, one `<change>\t<table>` line each, with `added`, `modified` or `deleted`. It prints `No uncommitted data changes.` when nothing differs.

It reports tables, not rows: a table with any inserted, updated or deleted row since the last checkpoint is `modified`, or `added` if the table is new. A row change that was later undone, such as an insert followed by a delete of the same key, still counts, just as it keeps `repodb status` dirty.

### `repodb commit -m <message>`

Checkpoints the journal: it turns the uncheckpointed changes into one Git data commit with the given message and prints its ID. With nothing to checkpoint, it prints the current data head and creates no commit.

| Flag | Default |
| --- | --- |
| `-m` | required |
| `--repo` | `.` |

## Synchronizing with a remote

RepoDB data travels only through these commands. `git push` and `git pull` move your source branches and never publish or merge RepoDB data (see [Git commands and RepoDB data](#git-commands-and-repodb-data)).

`sync`, `conflicts` and `resolve` use the remote recorded by `enable` (Git config `repodb.remote`). Pass `--remote` to override it. All four commands accept `--repo` (default `.`).

### `repodb enable --remote <name>`

Prepares a clone for synchronization with the named remote. It is safe to repeat. It:

1. adds the fetch refspec `+refs/repodb/data:refs/repodb/remotes/<name>/data` to the remote, keeping existing refspecs, and records `repodb.remote=<name>`;
2. fetches the remote's data history, if there is one, into that tracking ref and validates it.

Then:

- If the clone has no local data, it adopts the remote history (`adopted-remote`).
- If neither side has data, it creates an empty local history (`initialized-empty`).
- If both have data, the fetched history waits in the tracking ref until `repodb sync`.

`enable` adds no push refspec, installs no Git hooks and starts no process. `--remote` is required.

### `repodb sync`

Exchanges committed data history with the remote:

1. Fetches the remote data ref into the tracking ref, and validates the fetched snapshot and its SQL data.
2. If the local side is behind, fast-forwards the local data ref.
3. If the remote is behind, pushes with an ordinary fast-forward push, never a force push.
4. If both sides changed, performs a three-way merge and publishes a merge commit with both heads as parents, then pushes it. If the local ref or the remote moved during the merge, it starts again, up to three attempts.

Sync exchanges only committed data history, so uncheckpointed journal changes must become a data commit first:
- `repodb sync --commit -m <message>` checkpoints them with that message, then syncs. If a journal transaction commits while sync is running (for example from a running server), sync checkpoints again and retries, up to three attempts.
- In an interactive terminal without `--commit`, sync asks whether to checkpoint (`checkpoint before sync`), and then behaves like `--commit`.
- Otherwise, a dirty journal makes sync fail and leave the data ref unchanged; run it with `--commit -m`, or `repodb commit -m <message>` first. Scripts and CI should use `--commit -m`.

| Flag | Default |
| --- | --- |
| `--remote` | the configured remote |
| `--commit` | off |
| `-m` | required with `--commit` |
| `--repo` | `.` |

Output names the action taken: `up-to-date`, `fast-forwarded-local`, `pushed`, `merged`, or `conflicts`. On failure, it names both heads.

### `repodb conflicts`

After a sync that stopped on conflicts, lists each conflict with its ID, kind (`row` or `schema`), table, key, its current resolution (or `unresolved`), and the base, local and remote values (`<absent>` where a side has no value).

Conflicts are saved at `<git-common-dir>/repodb/conflicts/<remote>.json` and survive restarts. An unresolved merge never changes the local data ref.

### `repodb resolve --id <id> --take <choice>`

Records a resolution for one conflict:

| Choice | Result |
| --- | --- |
| `local` | keep the local row or table |
| `remote` | take the remote row or table |
| `base` | restore the common ancestor's version |
| `delete` | remove the row or table |

A choice applies to the whole row, or for schema conflicts to the whole table: fields are not merged. While conflicts remain, it prints how many are left. When every conflict has a choice, RepoDB re-checks that neither head has moved, publishes the merge, pushes it and removes the conflict file. If either head moved, the next `repodb sync` computes a fresh set of conflicts.

### Choosing primary keys for shared tables

The primary key is a row's identity across clones. If two clones insert different rows with the same key, sync reports an insert/insert conflict; rows are never renumbered. For tables written from several clones, use keys that are unique across writers, such as UUIDs or a composite key containing a writer ID. `AUTO_INCREMENT` is not supported (rdb-a2d28e).

## Git commands and RepoDB data

| Command | Source branches | RepoDB data |
| --- | --- | --- |
| `git clone` | cloned normally | not fetched until `repodb enable` |
| `git fetch` (after `enable`) | fetched normally | updates only the tracking ref `refs/repodb/remotes/<remote>/data` |
| `git pull` (merge or rebase) | fetched and integrated | tracking ref updated; nothing merged into local data |
| `git push` | your normal push rules | not pushed |
| `git checkout`, `merge`, `rebase` | as usual | unaffected |

There is no atomic link between a source commit and a data commit. Push your code and run `repodb sync`, as separate steps.

## Backup and recovery

- **Journal mode (the default)**: uncheckpointed changes exist only in `<git-common-dir>/repodb/working/v1/journal`; a copy of the Git repository alone restores only the last checkpoint. Back up in one of two ways:
  1. run `repodb commit -m <message>`, then back up the Git repository as for native-git mode; or
  2. stop RepoDB and copy the whole repository, including `.git`, which contains the journal.

  Each checkpoint compacts the journal down to the work since the checkpoint, so it stays small. Online backup is tracked in rdb-f33cb0.
- **Native-git mode (audit mode)**: the Git repository holds every committed transaction. A mirror clone (`git clone --mirror`, which includes `refs/repodb/data`), or a copy of `.git` taken while RepoDB is stopped, is a complete backup. A plain `git clone` does not include RepoDB data.
- Never delete `<git-common-dir>/repodb/working/`, `locks/` or `conflicts/` while RepoDB is running.

### Recovering a stranded journal

RepoDB never moves the data head while the journal has uncheckpointed transactions. If something outside RepoDB does, for example `git update-ref refs/repodb/data`, every open fails with `RepoDB committed head changed while working state is dirty: journal base <base>, data head <head>`. The journal's transactions are intact; they are based on `<base>`. To recover:

1. Make sure `<head>` is not lost: if it exists only in this repository, keep its ID (it is also in the reflog of `refs/repodb/data`).
2. `git update-ref refs/repodb/data <base>` puts the head back on the journal's base.
3. `repodb commit -m <message>` checkpoints the journal's transactions on that base.
4. `repodb sync` merges them with the remote through the usual three-way merge. If `<head>` came from the remote, this brings it back. Conflicts are reported and resolved as in any sync.

Linked Git worktrees share one data ref, one journal and one set of locks. RepoDB state belongs to the repository, not to a worktree.
