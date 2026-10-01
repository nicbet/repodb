# Architecture

RepoDB is a relational database stored in a Git repository. It lives on its own ref, separate from the source branches, so any clone can carry it. SQL runs through [go-mysql-server](https://github.com/dolthub/go-mysql-server), either in-process or behind a MySQL wire server. Tables are content-addressed Prolly trees whose nodes are Git blobs. Transactions become Git commits either one by one (native-git mode) or through a local journal that is checkpointed into commits (journal mode). Clones exchange data with an explicit sync that fast-forwards, pushes, or three-way merges by primary key.

This page describes how the pieces work today. Usage is in [cli.md](cli.md) and [library.md](library.md), the SQL surface in [sql.md](sql.md), and measured performance in [benchmarks/latest.md](benchmarks/latest.md).

## Packages

| Package | Responsibility |
| --- | --- |
| `engine` | The SQL catalog on go-mysql-server: tables, indexes, transactions and sessions. Also the key and row codecs, range scans, snapshot validation, three-way merge, and the `repodb_recover_commit` function. |
| `common/repository` | Repositories and snapshots: manifests, object inventories, publication to Git with commit outcomes and recovery, locking, and the journal (`WorkingState`). |
| `common/prolly` | Immutable, content-addressed Prolly trees: build, sorted streaming build, incremental `Apply`, seekable iterators, reachability. |
| `common/git` | Runs the `git` executable: object and tree writes with explicit fsync settings, `cat-file --batch` reads, ref updates, fetch and push. |
| `common/storage` | The content-addressed `Store` interface and SHA-256 hashes (`storage.Sum`). It also has in-memory and filesystem stores, which only tests use. |
| `common/robustio` | Rename and remove with retries for Windows sharing violations. |
| `integration` | `Enable`, `Sync`, `Conflicts`, `Resolve`: remote configuration, transport, merge and conflict records. |
| `server`, `client` | The MySQL wire server around an engine, and a Go client that decodes RepoDB commit errors. |
| `cmd/repodb`, `cmd/repodb-server` | The command-line programs. |

## Where state lives

All paths are under the repository's common Git directory (`git rev-parse --git-common-dir`), so linked worktrees share them.

| Location | Contents | Authority |
| --- | --- | --- |
| `refs/repodb/data` | the local data head: the latest published data commit | authoritative |
| `refs/repodb/remotes/<remote>/data` | the last fetched data head of a remote | fetched copy, never written locally |
| `repodb/working/v1/journal` | the journal: transactions not yet checkpointed | authoritative in journal mode |
| `repodb/locks/publish.lock` | serializes updates of `refs/repodb/data` | coordination |
| `repodb/locks/working.lock` | serializes journal appends and checkpoints | coordination |
| `repodb/conflicts/<remote>.json` | an interrupted merge's conflicts and chosen resolutions | local coordination state |
| `repodb/tmp/` | the temporary Git index used to build data trees | scratch |
| Git config `remote.<remote>.fetch`, `repodb.remote` | the data fetch refspec and the default sync remote | configuration |

RepoDB never touches source branches, `HEAD`, the user's index or the working tree.

## Snapshots

A data commit's tree is:

```text
manifest.json
objects/sha256/ab/cdef…   # one blob per RepoDB object, named by its SHA-256
```

`manifest.json` holds:
- `format_version` (currently 3);
- `default_database` (`repodb`);
- `tables`, mapping each table name to its schema root, data root and one root per secondary index;
- `objects`, the sorted, complete inventory of object hashes.

**Identities.** RepoDB identifies objects by SHA-256 of their content. The Git blob ID is a separate identity, so a repository may use Git's SHA-1 or SHA-256 object format.

**Commit parents.** A data commit's parent is the previous data head, and merges have two parents. SQL commits in native-git mode use the subject `RepoDB snapshot v3`; checkpoints use the given message.

**Opening a snapshot.** It must pass these checks before any use, or it fails with `repository.ErrCorrupt`:
- the manifest decodes strictly and the format version matches;
- the inventory is sorted and unique, and every table root is in it;
- the tree has exactly the listed entries;
- every object, read in one `git cat-file --batch` pass, hashes to its name.

The verified objects stay in memory for the snapshot's lifetime, so memory use grows with the size of the database. Snapshots from another format version are refused with an "unsupported RepoDB format" error. There is no migration between formats (rdb-92cd4a).

**SQL validation.** Before sync publishes a fetched snapshot, `engine.ValidateSnapshot` decodes every schema and walks every Prolly tree. Its results are cached per repository, commit and journal generation.

## Tables

Each table has three kinds of object:

- **A schema object:** JSON describing columns (type, length, precision, scale, collation, enum values, defaults, nullability), primary-key ordinals, check constraints and index definitions.
- **A data tree:** a Prolly tree mapping the encoded primary key to the encoded row.
- **One tree per secondary index:** mapping the encoded index key to the primary key.

### Prolly trees

A Prolly tree is a B-tree-like structure whose node boundaries depend only on content:
- Nodes are JSON, stored as content-addressed objects.
- Entries are split into chunks by a rolling hash over their FNV-64 fingerprints. A chunk ends once it has at least 32 entries and the rolling hash's low 6 bits are zero, or at 128 entries.
- Interior levels chunk their child links the same way.

The same set of entries always produces the same root, whatever order of edits built it. That property makes whole-table comparison cheap (equal roots mean equal tables) and lets unchanged subtrees be shared between commits.

Writes use `prolly.Apply`, which rewrites only the chunks an edit touches. Merge uses a streaming sorted builder. Reads use iterators that can seek to a key and keep one node per level in memory.

### Key encoding

Keys are the concatenation of one self-delimiting, order-preserving encoding per column (`engine/keycodec.go`), so comparing encoded keys bytewise agrees with SQL order:

| SQL type | Encoding |
| --- | --- |
| signed integers | 8 bytes big-endian, sign bit flipped |
| unsigned integers | 8 bytes big-endian |
| ENUM | 2-byte big-endian index |
| FLOAT, DOUBLE | 8-byte IEEE-754 bits: all bits flipped if negative, else the sign bit flipped. −0 is normalized to 0; NaN is rejected. |
| DECIMAL | a sign-class byte, then for non-zero values a sign-flipped 4-byte exponent and digit bytes ending in `0x00`. Bitwise inverted for negatives. `1.0` and `1.00` encode equally. |
| DATE, DATETIME, TIMESTAMP, TIME | 8-byte microseconds (since the Unix epoch in UTC, or as a duration), sign bit flipped |
| CHAR, VARCHAR, TEXT | UTF-8 bytes for binary collations, else a 4-byte weight per rune from the collation. CHAR drops trailing spaces. `0x00` is escaped as `0x00 0xFF` and the value ends with `0x00 0x01`. |
| BINARY, VARBINARY, BLOB | raw bytes, escaped and terminated like strings |

**Index keys.** A secondary-index key writes `0x00` for a NULL column, or `0x01` plus the column's encoding, so NULLs sort first. It appends the primary key when the index is not unique or a column is NULL. Keys are never decoded: values are read from the row.

### Row encoding

A row (`engine/rowcodec.go`) is:
- a uvarint column count;
- a NULL bitmap of `ceil(n/8)` bytes;
- one cell per non-NULL column.

| SQL type | Cell |
| --- | --- |
| signed integers, TIME | zigzag varint |
| unsigned integers, ENUM index | uvarint |
| FLOAT / DOUBLE | 4 / 8 bytes little-endian IEEE-754 bits |
| DATE, DATETIME, TIMESTAMP | 8 bytes little-endian microseconds since the Unix epoch (UTC) |
| DECIMAL | the unscaled value at the column's scale: `0x00` + zigzag varint if it fits in int64, else `0x01`, a sign byte and a length-prefixed big-endian magnitude |
| JSON | uvarint length + canonical MySQL JSON text |
| strings, binary | uvarint length + bytes |

The schema describes the cells, so rows carry no type tags. Every encoding is a function of the value, so equal rows have equal bytes. Merge relies on this.

## Reads

**Pinned snapshots.** A transaction pins a snapshot at its first table access and reads only that snapshot plus its own uncommitted edits. Readers take no locks.

**Freshness checks.** At each transaction boundary an engine checks whether the data ref or the journal changed:
- a `stat` of the ref storage and the journal file;
- reading only journal frames it has not seen.

So engines in other processes, and the server, observe each other's commits.

**Access paths:**
- Point lookups on the primary key read the data tree directly.
- Ranges and secondary-index lookups iterate over key intervals, merging the tree with the transaction's and the journal's pending edits. Secondary-index lookups then fetch rows by primary key.
- Full scans stream the data tree in key order and decode only the rows they return. A `LIMIT` stops the iteration early.
- Iteration is forward-only, so `ORDER BY … DESC` sorts (rdb-acd36d).

## Writes and persistence modes

A transaction collects its row and schema edits in memory. A failing statement undoes its own edits. What happens at `COMMIT` depends on the persistence mode.

### Native-git

1. The engine applies each changed table's edits to its trees with `prolly.Apply`, in memory. A table is rebuilt only when DDL rewrote it.
2. It validates the new snapshot's object inventory.
3. Under `publish.lock`, it:
   1. writes the new objects as Git blobs through a temporary index;
   2. writes a tree and a commit;
   3. advances `refs/repodb/data` with `git update-ref <new> <expected-old>`.
4. It reloads and verifies the published snapshot.

If the ref no longer points at the transaction's base, the update fails with `repository.ErrConflict` and nothing becomes visible. An interruption before the ref update leaves only unreachable objects, which `git gc` removes.

### Journal

The journal is an append-only file of framed records:

```text
"RDBJ" | payload length (u32 BE) | CRC-32C (u32 BE) | JSON payload
```

**Commit.** Under `working.lock`, a committing transaction checks that its base (the journal generation and the committed data head) is still current, or fails with `ErrConflict`. It then appends a `typed-prepare` record and a `commit` record, and `fsync`s before returning success. The prepare record carries the transaction's typed edits:
- per table, a new schema object or a drop;
- the changed rows as encoded key and value, or delete.

Each transaction gets a random transaction ID and the next generation number. Git is not touched.

**Replay** loads the last checkpointed commit and applies matched prepare/commit pairs after it. Only the base snapshot and the transactions after the last checkpoint are loaded:
- A short final frame is an incomplete tail. It is ignored, and truncated before the next append.
- A bad checksum or out-of-order history in complete frames is corruption (`ErrWorkingCorrupt`).
- A long-lived engine remembers the verified offset and the file's identity. It reads only new frames, and falls back to a full replay if the file was replaced or shortened.

**Pending edits.** Pending row edits are kept as an overlay on the checkpointed trees, and reads merge them in. They become Prolly trees only at checkpoint.

**Checkpoint** (`repodb commit`, `Engine.Checkpoint`), under `working.lock`:
1. applies the pending edits to the trees;
2. publishes one data commit through the native-git path;
3. appends a `checkpoint` record linking the generation to the Git commit.

If the process dies after publishing but before the record, the next open sees that the published snapshot equals the working state and treats the journal as clean.

**Guards:**
- A native-git engine refuses to open while the journal is dirty (`ErrWorkingStateDirty`).
- If the data head moves under a dirty journal, loading fails with `ErrWorkingBaseChanged`. Sync can currently cause this (rdb-e0c717).

**Growth.** The journal is never compacted and grows with every transaction (rdb-515fae).

### Concurrency limits

Commits are optimistic and repository-wide: any commit since a transaction's snapshot rejects that transaction, whichever rows it touched. Within one repository, commits are serialized by `working.lock` (journal) or `publish.lock` (native-git), and each journal commit does its own `fsync`. Together these bound concurrent write throughput. Per-key conflict detection, group commit and automatic retry are tracked in rdb-df092b.

## Sync and merge

`integration.Sync`:
1. refuses a dirty journal;
2. fetches the remote's `refs/repodb/data` into the tracking ref;
3. validates the fetched snapshot and its SQL data;
4. compares ancestry with the local head.

It then does one of the following:

- **Fast-forward local.** Under `publish.lock`, the local ref moves to the fetched commit.
- **Push.** The local commit is pushed to the remote's `refs/repodb/data` as an ordinary fast-forward push, without force. The push does not hold `publish.lock`, so local SQL commits continue during a slow push.
- **Merge.** A three-way merge between the local head, the fetched head and their Git merge base.

**How merge compares.** Merge works table by table and key by key:
- It accepts identical results, changes made on only one side, and changes to different keys.
- These become conflicts:
  - different values written for the same key on both sides;
  - an update on one side and a delete on the other;
  - a table created, changed or dropped incompatibly on both sides.

Row conflicts are compared on whole encoded rows, so fields are never merged. Unchanged tables are recognized by equal roots and reused without reading their rows. Merged trees are streamed through a sorted builder, and secondary indexes are rebuilt from the merged rows. Every merged row is decoded and checked before publication.

**Publishing a merge.** A merge without conflicts is published under `publish.lock` as a two-parent commit and pushed. If the local ref moved or the push was rejected, sync fetches and merges again, up to three attempts.

**Conflicts.** A merge with conflicts writes `conflicts/<remote>.json` (the three heads, each conflict and any chosen resolutions) and leaves the local ref unchanged. `Resolve` records one choice at a time. When every conflict has one, it re-checks that both heads are unchanged, merges with those choices, publishes and pushes, then deletes the conflict file. If a head moved, the next sync computes new conflicts.

`Enable` adds the fetch refspec and `repodb.remote`. It adopts the remote history when there is no local data, and initializes an empty history when neither side has data.

## Durability and recovery

**Git durability settings.** Git runs with `-c core.fsync=committed,reference -c core.fsyncMethod=fsync` for object, tree, commit and ref writes. RepoDB reports success only after the compare-and-swap ref update has returned and the new snapshot verified. These guarantees assume the filesystem and storage honor `fsync` and atomic ref replacement.

**Commit outcomes.** Native-git publication returns one of three outcomes:
- `rejected`: nothing changed;
- `committed`: published, even if a later check failed;
- `unknown`: RepoDB could not tell, for example when the ref update was interrupted.

A `committed` or `unknown` outcome carries the candidate commit ID. `Repository.RecoverCommit` settles an `unknown` outcome by checking whether the candidate is the data head or an ancestor of it. SQL exposes the same check as `repodb_recover_commit`. A journal commit that fails after its records reached disk returns `committed` with the transaction ID, and `WorkingState.RecoverTransaction` settles uncertain journal outcomes.

**Repository states:**

| State | Error |
| --- | --- |
| no `refs/repodb/data` | `repository.ErrNotInitialized` |
| another format version | unsupported-format error |
| missing, extra or altered snapshot content | `repository.ErrCorrupt` |
| stale transaction base | `repository.ErrConflict` (nothing written) |
| damaged journal history | `repository.ErrWorkingCorrupt` |
| data head moved under a dirty journal | `repository.ErrWorkingBaseChanged` |

**Garbage collection.** Published data is reachable from `refs/repodb/data`, so `git gc` keeps it. Uncheckpointed journal data exists only in the journal file.

## Git integration

RepoDB installs no Git hooks:
- Git has no hook that runs on every fetch.
- `post-merge` misses rebases.
- `pre-push` cannot add refs to a push.

Hooks would also compete with existing hook managers. So `repodb sync` is the one operation that moves data, and source and data histories are never updated atomically together. Pushing the data ref runs without the host repository's `pre-push` hook.

RepoDB works with SHA-1 and SHA-256 Git repositories. Linked worktrees share the data ref, journal, locks and conflict files, because all of them live in the common Git directory.

## Platforms

- **macOS:** the tested platform (Git 2.55).
- **Linux:** shares the Unix code paths (`flock(2)` locks) but has no separate test run.
- **Windows:** builds and passes `go vet` and staticcheck in `make test` and `make lint`. The test suite has not been run there. Windows uses `LockFileEx` locks and retried renames and removals.

The test surface is described in [testing.md](testing.md).
