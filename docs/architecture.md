# Architecture

RepoDB is a relational database stored in a Git repository. It lives on its own ref, separate from the source branches, so any clone can carry it. SQL runs through [go-mysql-server](https://github.com/dolthub/go-mysql-server), either in-process or behind a MySQL wire server. Tables are content-addressed Prolly trees whose nodes are Git blobs. Transactions become Git commits either one by one (native-git mode) or through a local journal that is checkpointed into commits (journal mode). Clones exchange data with an explicit sync that fast-forwards, pushes, or three-way merges by primary key.

This page describes how the pieces work today. Usage is in [cli.md](cli.md) and [library.md](library.md), the SQL surface in [sql.md](sql.md), and measured performance in [benchmarks/latest.md](benchmarks/latest.md).

## Packages

| Package | Responsibility |
| --- | --- |
| `engine` | The SQL catalog on go-mysql-server: tables, indexes, transactions and sessions. Also the key and row codecs, range scans, snapshot validation and on-demand integrity checks (`Check`), three-way merge, and the `repodb_recover_commit` function. |
| `common/repository` | Repositories and snapshots: manifests, object inventories, publication to Git with commit outcomes and recovery, locking, and the journal (`WorkingState`). |
| `common/prolly` | Immutable, content-addressed Prolly trees: build, sorted streaming build, incremental `Apply`, seekable iterators, reachability. |
| `common/git` | Runs the `git` executable: object and tree writes with explicit fsync settings, object reads through a long-lived `cat-file --batch` process, ref updates, fetch and push. |
| `common/storage` | The content-addressed `Store` interface and SHA-256 hashes (`storage.Sum`), plus an in-memory store. `Get` returns the stored bytes themselves, shared with other readers, so callers never modify them. |
| `common/robustio` | Rename and remove with retries for Windows sharing violations. |
| `integration` | `Enable`, `Sync`, `Conflicts`, `Resolve`: remote configuration, transport, merge and conflict records. |
| `server`, `client` | The MySQL wire server around an engine, and a Go client that decodes RepoDB commit errors. |
| `cmd/repodb`, `cmd/repodb-server` | The command-line programs. |

## Where state lives

All paths are under the repository's common Git directory (`git rev-parse --git-common-dir`), so linked worktrees share them.

| Location | Contents | Authority |
| --- | --- | --- |
| `refs/repodb/data` | the local data head: the latest published data commit | authoritative |
| `refs/repodb/remotes/<remote>/data` | the remote's data head as last seen by a fetch or by RepoDB's own push | copy of the remote; Git's fetch and push update it, RepoDB never writes it directly |
| `repodb/working/v1/journal` | the journal: an anchor for the last checkpoint, then the transactions since | authoritative in journal mode |
| `repodb/locks/publish.lock` | serializes updates of `refs/repodb/data` | coordination |
| `repodb/locks/working.lock` | serializes journal appends and checkpoints; every update of `refs/repodb/data` takes it before `publish.lock` | coordination |
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
- `format_version` (currently 6);
- `default_database` (`repodb`);
- `tables`, mapping each table name to its schema root, data root and one root per secondary index;
- `objects`, the sorted, complete inventory of object hashes.

**Identities.** RepoDB identifies objects by SHA-256 of their content. The Git blob ID is a separate identity, so a repository may use Git's SHA-1 or SHA-256 object format.

**Commit parents.** A data commit's parent is the previous data head, and merges have two parents. SQL commits in native-git mode use the subject `RepoDB snapshot v6` (the storage format version); checkpoints use the given message.

**Opening a snapshot.** It must pass these checks before any use, or it fails with `repository.ErrCorrupt`:
- the manifest decodes strictly and the format version matches;
- the inventory is sorted and unique, and every table root is in it;
- the tree has exactly the listed entries.

Opening reads the manifest and lists the tree (`ls-tree`), nothing more. Objects are read when first used, by Git object ID, and each must hash to its name before it is cached or returned; otherwise the read fails with `repository.ErrCorrupt`, as it does for a missing blob. So a corrupt object is reported by the first read that reaches it, not when the snapshot opens. Sync still checks every object of a fetched snapshot before publishing it, or relies on an earlier check of the same Git object (see SQL validation below). `repodb check` (`engine.Check`) checks every object on demand; see below. Prolly walks and full-tree iterators hint the store to read an internal node's children in one batch (`storage.Prefetcher`). The objects read stay in memory, shared by every snapshot of the commit, so memory grows with what is read rather than with the database. Snapshots from another format version are refused with an "unsupported RepoDB format" error. There is no migration between formats (rdb-92cd4a).

**Reading objects.** Each worktree has one long-lived `git cat-file --batch` process. It serves the manifest and object reads that miss a snapshot's cache, so a read starts no process. Requests are serialized. An I/O or protocol error, or a cancelled context, ends the process; the next read starts a new one, and a read that finds an idle process dead retries once. The process exits after 5 seconds without requests, and `engine.Close` stops it at once (`git.CloseReaders`), because on Windows it keeps pack files open. It sees objects written after it started: Git finds new loose objects and re-scans packs when an object is missing.

**Reusing loaded snapshots.** A process-wide memo maps each repository and commit to its checked inventory and the cache of objects read so far. It holds weak references, so it never keeps a snapshot alive itself. While any caller still holds a snapshot of a commit (an engine, the journal's replayed view, sync), opening that commit again returns a new `Snapshot` sharing that content, without Git reads or hashing. The journal's working views derived from a commit hold it too, so a checkpoint finds its base without reloading it. Every caller gets its own `Snapshot`, because callers set its working generation. The memo trusts objects checked earlier in the process: a commit ID names immutable content, and corruption that appears on disk after an object was read is not detected until the snapshot is collected and read again, or until `repodb check` runs. A published commit's snapshot is memoized too, so loading the commit just checkpointed costs nothing while its result is held.

**SQL validation.** Before sync publishes a fetched snapshot, `engine.ValidateSnapshot` decodes every schema and walks every Prolly tree. It also checks every row's encoding and primary key, streaming each data tree without keeping rows in memory. Validation reads every object reachable from a table's roots, so each is checked against its name; for a table without rows it reads the schema. Results are cached per table, keyed by repository and the table's schema, data and index roots, not by commit. Each entry records the objects with the Git object IDs they were read from. A cached result counts for a snapshot only if that snapshot provides the same objects: each is already read and checked in that snapshot, or stored under the same Git object ID. So an object held only in a local journal, or stored under another blob in a fetched commit, never vouches for a snapshot that lacks it, and that table is validated again. Journal commits, checkpoints and pulls re-validate only the tables whose roots changed. The cache keeps at most 1,024 tables and 1,048,576 recorded objects, evicting the least recently used. Within a changed table, validation skips subtrees it has validated before. A second cache records each validated Prolly node with its entry count, its children and the Git object ID it was read under. Data-tree nodes are keyed by the table's schema root as well, because rows valid under one schema may not be valid under another, so a schema change validates every row again; index-tree nodes need only structural checks and are keyed without one. A cached subtree counts for a snapshot under the same rule as a cached table: the snapshot must provide every node of it, and the parent still checks the subtree's entry count against its child link. A one-row change therefore reads and checks only the changed path of each tree. This cache holds at most 1,048,576 nodes plus child links, evicting the least recently used; a subtree with an evicted node is read and validated again. Pending journal edits are not part of this validation.

**Checking on demand.** `engine.Check`, behind `repodb check`, verifies the data head, a given commit or the whole data history without trusting earlier work in the process. For each commit it opens the snapshot through `Repository.LoadSnapshotUncached`, which runs the open-time checks but bypasses the snapshot memo. `Snapshot.VerifyObjects` then reads every listed object in batches of 1,024 through `git cat-file`, and checks each against its name. It reports every missing, non-blob or mismatched object rather than stopping at the first, and caches the good bytes for the next step. Last, every table is validated as above, but against a validation cache private to the run, so the process-wide caches neither vouch for nor learn from the check. Within one run the usual rules apply: a Git object verified for one commit isn't read again for another, and a table or subtree validated for one commit is reused for another only if that commit provides the same Git objects. Objects that a commit lists but no table reaches are reported as warnings. Each commit's snapshot is released before the next commit is checked.

## Tables

Each table has three kinds of object:

- **A schema object:** JSON describing columns (type, length, precision, scale, collation, enum values, defaults, nullability), primary-key ordinals, check constraints and index definitions.
- **A data tree:** a Prolly tree mapping the encoded primary key to the encoded row.
- **One tree per secondary index:** mapping the encoded index key to the primary key.

### Prolly trees

A Prolly tree is a B-tree-like structure whose node boundaries depend only on content:
- Nodes are binary, stored as content-addressed objects (see [Node encoding](#node-encoding)).
- Entries are split into chunks by a per-entry boundary hash of the entry's **key**: FNV-64a passed through the splitmix64 finalizer. A chunk ends after an entry once it has at least 64 entries and that entry's hash has its low 6 bits zero, or at 256 entries. Leaves average about 124 entries whatever the key format, and fewer than 5% end at the cap. (FNV-64a's raw low bits follow the low bits of the key's last byte, so without the finalizer sequential integer keys would cut at a fixed period.)
- Interior levels chunk their child links the same way, hashing each link's maximum key.
- The decision never looks at values, child hashes or earlier entries. So an update never moves a boundary, and an insert or delete moves at most the boundaries up to the next key-hash boundary.

The same set of entries always produces the same root, whatever order of edits built it. That property makes whole-table comparison cheap (equal roots mean equal tables) and lets unchanged subtrees be shared between commits.

Writes use `prolly.Apply`, which rewrites only the chunks an edit touches: for an update, one leaf and its path to the root. Merge uses a streaming sorted builder. Reads use iterators that can seek to a key and keep one node per level in memory.

### Node encoding

A node (`common/prolly/codec.go`) is:

```text
'P' | codec version (1) | level (u8) | uvarint item count | items
```

- A **leaf** item (level 0) is a uvarint key length, the key, a uvarint value length and the value.
- An **interior** item is a uvarint max-key length, the child's maximum key, a uvarint count of the leaf entries below the child, and the child's 32-byte SHA-256.

Nodes are read in place: iterators parse a leaf one entry at a time, and the keys and values they return point into the stored bytes rather than copies. Point reads (`Tree.Get`) binary-search each node on the path. Entries are length-prefixed, so a node can't be searched directly. Instead, the first read of a node parses it once (rejecting malformed framing and trailing bytes) and caches its item start offsets, keyed by node hash. Nodes are immutable, so the cached offsets are valid in every store and snapshot. The cache is process-wide and bounded (16 shards of 4,096 nodes, arbitrary eviction). Only the offsets are cached, because the store already holds the node bytes. The counts make a tree's size a read of its root (`Tree.Count`). Validation (`prolly.Reachable`) checks every link's count against its subtree, along with key order and bounds; a decoder rejects truncated nodes and trailing bytes.

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
- Every access path decodes only the columns the query uses: go-mysql-server pushes the needed columns down (`sql.ProjectedTable`), and the row decoder skips the other cells. A scan that needs no column, such as the counted side of a join, decodes no rows.
- Stored keys are not re-derived from rows during reads: snapshot validation checks every stored row's key once per snapshot.
- `SELECT COUNT(*) FROM t` reads the row count from the data tree's root when the transaction sees no pending edits (`sql.StatisticsTable`); otherwise it counts while scanning.
- Scans run in either direction. For `ORDER BY <index columns> DESC`, go-mysql-server asks for a reverse lookup, and RepoDB walks the tree, the pending journal edits and the transaction's own edits from the end of each key interval, visiting intervals last to first. A reverse scan decodes each leaf whole, because entries are length-prefixed only forward.

## Writes and persistence modes

A transaction collects its row and schema edits in memory. A failing statement undoes its own edits. What happens at `COMMIT` depends on the persistence mode.

### Native-git

1. The engine applies each changed table's edits to its trees with `prolly.Apply`, in memory. A table is rebuilt only when DDL rewrote it.
2. It validates the new snapshot's object inventory.
3. Under `working.lock`, it checks that the journal has no uncheckpointed transactions, or fails with `ErrWorkingStateDirty`. Then, also under `publish.lock`, it:
   1. writes the new objects as Git blobs through a temporary index;
   2. writes a tree and a commit;
   3. advances `refs/repodb/data` with `git update-ref <new> <expected-old>`.
4. It reloads and verifies the published snapshot.

If the ref no longer points at the transaction's base, the update fails with `repository.ErrConflict` and nothing becomes visible. The base is also checked as soon as the locks are held, so a writer that has already lost the race is rejected before it writes any Git object and doesn't keep later writers waiting. An interruption before the ref update leaves only unreachable objects, which `git gc` removes.

### Journal

The journal is an append-only file of framed records:

```text
"RDBJ" | payload length (u32 BE) | CRC-32C (u32 BE) | JSON payload
```

**Commit.** Under `working.lock`, a committing transaction checks that its base is still valid (see [Write conflicts](#write-conflicts)), or fails with `ErrConflict`. It then appends a `typed-prepare` record and a `commit` record, and flushes the journal at the engine's durability level (see [Durability and recovery](#durability-and-recovery)) before returning success. The prepare record carries the transaction's typed edits:
- per table, a new schema object or a drop;
- the rows this transaction changed, as encoded key and value, or delete. Replay merges records key by key, so a record never repeats earlier transactions' edits;
- per table, the keys this transaction added to `UNIQUE` indexes (`claims`), for conflict detection only.

The edits are applied on top of the current state, which may already include transactions committed after this one's snapshot.

Each transaction gets the next generation number and a transaction ID of the form `<generation>-<random>`. Git is not touched.

**Replay** loads the last checkpointed commit and applies matched prepare/commit pairs after it. Only the base snapshot and the transactions after the last checkpoint are loaded:
- A short final frame is an incomplete tail. It is ignored, and truncated before the next append.
- A bad checksum or out-of-order history in complete frames is corruption (`ErrWorkingCorrupt`).
- A long-lived engine remembers the verified offset and the file's identity: its inode and its first frame header. It reads only new frames, and falls back to a full replay if the file was replaced or shortened. The header check catches a replacement that reuses the old inode number.

**Pending edits.** Pending row edits are kept as an overlay on the checkpointed trees, and reads merge them in. They become Prolly trees only at checkpoint. Each table's overlay is a list of immutable sorted runs, newest first: a commit adds its edits as a new run and merges runs of similar size, so snapshots and transactions share the overlay without copying it. Rows in the overlay stay encoded and are decoded when read. A transaction's own edits are layered on top. Secondary indexes keep their pending entries the same way: a shared set per index, derived once per journal generation and cached, under the transaction's own appended entries, which a failed statement truncates. The cost of a commit, a read or an index lookup therefore doesn't grow with the number of edits since the last checkpoint.

**Checkpoint** (`repodb commit`, `Engine.Checkpoint`), under `working.lock`:
1. applies the pending edits to the trees;
2. publishes one data commit through the native-git path;
3. appends a `checkpoint` record linking the generation to the Git commit;
4. compacts the journal: writes a new file holding a single **anchor** (the `checkpoint` record, plus the IDs of the transactions it replaces), `fsync`s it, renames it over the journal and `fsync`s the directory.

If the process dies after publishing but before the record, the next open sees that the published snapshot equals the working state and treats the journal as clean. Compaction comes after the checkpoint is published and recorded, so a crash or error during it leaves the old journal, which is still valid, and the next checkpoint compacts it. A failure doesn't fail the checkpoint; `WorkingMetrics.JournalCompactionFailures` counts it. A replay that starts with an anchor continues from the anchor's generation.

**Guards.** A dirty journal's transactions are based on the current data head, so the head must not move until a checkpoint publishes them:
- Every update of `refs/repodb/data` takes `working.lock` and then `publish.lock`, and holds both until the ref update. Only a checkpoint may publish while the journal is dirty.
- Native-git commits and sync's fast-forward and merge publications check the journal under `working.lock` and fail with `ErrWorkingStateDirty` while it is dirty. The check reads only the record kinds appended since its last call, so an unchanged journal costs one `stat`.
- A native-git engine also refuses to open while the journal is dirty.
- If the data head moves under a dirty journal anyway, for example through an external `git update-ref`, loading fails with `ErrWorkingBaseChanged`, naming the journal's base and the head. [cli.md](cli.md#recovering-a-stranded-journal) describes the recovery.

**Growth.** The journal holds only the work since the last checkpoint, plus the anchor, so its size doesn't grow with the repository's age. Retention policies, archiving and a manual `repodb compact` are tracked in rdb-515fae.

### Write conflicts

Journal commits use first-committer-wins snapshot isolation. A transaction whose snapshot is older than the journal's current generation still commits if its writes are disjoint from those of every transaction committed since. Otherwise it fails with `ErrConflict`, naming the table and the kind of overlap. The check runs under `working.lock`:
1. If the data head has moved since the snapshot (a checkpoint), the transaction conflicts.
2. **Rows.** The pending edits record, for each row key, the generation that last wrote it. Writing a key that was written after the snapshot is a conflict. A primary-key change writes both the old and the new key.
3. **Unique indexes.** A transaction's `claims` are recorded with their generation. Claiming a key that was claimed after the snapshot, even for a different row, is a conflict. A unique index key that contains a NULL also contains the primary key, so it never collides.
4. **Schema.** A table created, altered or dropped after the snapshot conflicts with any write to it. A transaction that creates, alters or drops a table conflicts if anything wrote to that table after its snapshot.

The table and claim generations live in memory, in a write log kept with the replayed view. Replay rebuilds it, so commits by other processes are checked too. It covers the transactions since the last checkpoint or whole-manifest commit; a snapshot older than that always conflicts. Reads are not tracked, so write skew is possible ([sql.md](sql.md#transactions)).

The engine caches each generation's derived secondary-index entries. A rebased transaction's index entries were computed on its snapshot, but because its rows are disjoint from everything committed since, its own entries are still correct on top of the current generation. They are added to that generation's cache, if it is cached, instead of being derived again.

### Concurrency limits

Commits are optimistic. In journal mode a transaction conflicts only with overlapping writes ([Write conflicts](#write-conflicts)). In native-git mode it conflicts with any commit since its snapshot, whichever rows that commit touched. Within one repository, commits are serialized by `working.lock` (journal) or by `working.lock` and `publish.lock` (native-git), and each journal commit does its own flush. Together these bound concurrent write throughput. Group commit and automatic retry are tracked in rdb-df092b.

## Sync and merge

`integration.Sync`:
1. refuses a dirty journal (with `SyncOptions.Checkpoint`, it checkpoints it instead);
2. fetches the remote's `refs/repodb/data` into the tracking ref. It first reads the remote head with `ls-remote`, and skips the fetch, with its transfer and Git auto-maintenance, when the tracking ref already names that commit and the commit is present locally;
3. validates the fetched snapshot and its SQL data;
4. compares ancestry with the local head through one `git merge-base --all`: the remote is behind when its head is the only merge base, the local side when its head is.

Snapshots of commits already loaded in the process are reused rather than read again (see [Snapshots](#snapshots)).

It then does one of the following:

- **Fast-forward local.** Under `working.lock` and `publish.lock`, the local ref moves to the fetched commit, unless a journal transaction committed since step 1 (`ErrWorkingDirty`; with `SyncOptions.Checkpoint`, sync checkpoints and retries).
- **Push.** The local commit is pushed to the remote's `refs/repodb/data` as an ordinary fast-forward push, without force. The push does not hold `publish.lock`, so local SQL commits continue during a slow push. A successful push also moves the tracking ref to the pushed commit (Git updates remote-tracking refs that match the fetch refspec), so sync does not fetch afterwards.
- **Merge.** A three-way merge between the local head, the fetched head and their Git merge base.

**How merge compares.** Merge works table by table and key by key:
- It accepts identical results, changes made on only one side, and changes to different keys.
- These become conflicts:
  - different values written for the same key on both sides;
  - an update on one side and a delete on the other;
  - a table created, changed or dropped incompatibly on both sides.

Row conflicts are compared on whole encoded rows, so fields are never merged. Unchanged tables are recognized by equal roots and reused without reading their rows. Merged trees are streamed through a sorted builder, and secondary indexes are rebuilt from the merged rows. Every merged row is decoded and checked before publication.

**Publishing a merge.** A merge without conflicts is published as a two-parent commit, under the same locks and journal check as a fast-forward, and pushed. If the local ref moved or the push was rejected, sync fetches and merges again, up to three attempts.

**Conflicts.** A merge with conflicts writes `conflicts/<remote>.json` (the three heads, each conflict and any chosen resolutions) and leaves the local ref unchanged. `Resolve` records one choice at a time. When every conflict has one, it re-checks that both heads are unchanged, merges with those choices, publishes and pushes, then deletes the conflict file. If a head moved, the next sync computes new conflicts.

`Enable` adds the fetch refspec and `repodb.remote`. It adopts the remote history when there is no local data, and initializes an empty history when neither side has data.

## Durability and recovery

**Journal commit durability.** Each engine flushes its journal commits at one of three levels (`engine.Options.Durability`, `--durability`):

| Level | macOS | Linux | Windows | An acknowledged commit survives |
| --- | --- | --- | --- | --- |
| `full` | `F_FULLFSYNC` | `fdatasync` | `FlushFileBuffers` | power loss |
| `normal` (default) | `F_BARRIERFSYNC` | `fdatasync` | `FlushFileBuffers` | process and OS crashes; a power loss may lose the newest commits |
| `off` | no flush | no flush | no flush | process crashes; an OS crash or power loss may lose commits the OS had not written back yet |

On Linux and Windows, `normal` is the same full flush as `full`. On macOS, `F_FULLFSYNC` forces the drive's cache to stable media and costs about 5 ms per commit; `F_BARRIERFSYNC` hands the data to the drive in order without forcing its cache, for about 0.25 ms. MySQL, PostgreSQL and SQLite also don't force the drive cache by default on macOS.

No level can corrupt the journal. It is append-only, every record is length-prefixed and checksummed, and replay truncates a partial final record. Flushes keep records in order, so a crash can only cut off the newest commits, never leave a gap. Whatever the level, a checkpoint fully flushes the journal before publishing its Git commit, and compaction fully flushes the new file. A published checkpoint therefore never outlives, in a power loss, the journal records it contains.

**Git durability settings.** Git runs with `-c core.fsync=committed,reference -c core.fsyncMethod=fsync` for object, tree, commit and ref writes. RepoDB reports success only after the compare-and-swap ref update has returned and the new snapshot verified. These guarantees assume the filesystem and storage honor `fsync` and atomic ref replacement.

**Commit outcomes.** Native-git publication returns one of three outcomes:
- `rejected`: nothing changed;
- `committed`: published, even if a later check failed;
- `unknown`: RepoDB could not tell, for example when the ref update was interrupted.

A `committed` or `unknown` outcome carries the candidate commit ID. `Repository.RecoverCommit` settles an `unknown` outcome by checking whether the candidate is the data head or an ancestor of it. SQL exposes the same check as `repodb_recover_commit`. A journal commit that fails after its records reached disk returns `committed` with the transaction ID, and `WorkingState.RecoverTransaction` settles uncertain journal outcomes. After compaction it still answers exactly for transactions from the current and the previous checkpoint interval; for older ones it returns `unknown` with `ErrWorkingHistoryTruncated`, never a wrong `rejected`.

**Repository states:**

| State | Error |
| --- | --- |
| no `refs/repodb/data` | `repository.ErrNotInitialized` |
| another format version | unsupported-format error |
| missing, extra or altered snapshot content | `repository.ErrCorrupt` |
| transaction conflicting with one committed since its snapshot | `repository.ErrConflict` (nothing written) |
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
