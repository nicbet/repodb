# RepoDB

RepoDB is an experiment in putting a MySQL-compatible database inside an
existing Git repository. It is written in Go and combines:

- `go-mysql-server` for SQL analysis, planning, and execution;
- DoltHub's Vitess fork for MySQL syntax and the wire protocol;
- an immutable, content-addressed Prolly tree for table data; and
- ordinary Git commits for history, branching, transport, and collaboration.

## Status

This repository contains the first executable vertical slice. The MySQL server
accepts DDL and DML, the client speaks the MySQL protocol, and the common module
can build and read deterministic Prolly trees persisted beneath `.repodb/`.

SQL tables currently use the `go-mysql-server` in-memory adapter. They are **not
yet persisted** to Prolly roots. That adapter boundary is the next milestone.

## Layout

```text
cmd/repodb/          CLI: initialize, inspect, snapshot, and query
cmd/repodb-server/   MySQL-compatible server process
client/              reusable MySQL wire client
server/              go-mysql-server host and storage adapters
common/prolly/       deterministic content-defined tree construction
common/storage/      content-addressed memory and filesystem stores
common/repository/   .repodb format, manifests, and repository discovery
common/git/          narrow Git CLI boundary
common/query/        Vitess parser boundary
```

These are Go packages in one Go module. Keeping one module initially avoids
cross-module release/version churn; `server`, `client`, and `common` can become
separate Go modules later without changing their architectural boundaries.

## Try it

Go 1.27 or newer is expected by the current experiment.

```sh
make test
make build
./bin/repodb init .
./bin/repodb-server -repo .
```

In a second terminal, either use any MySQL client or the included one:

```sh
./bin/repodb sql 'SELECT 1 + 1 AS answer'
mysql --host=127.0.0.1 --port=3306 --user=root repodb
```

Once persistent table adapters land, database state will be staged and
committed explicitly:

```sh
./bin/repodb status
./bin/repodb snapshot -m 'Update application data'
```

RepoDB never commits unrelated worktree changes: snapshot commands restrict Git
operations to `.repodb`.

## On-disk model

```text
.repodb/
  config.json
  manifest.json              # table name -> schema root and data root
  objects/sha256/ab/cdef...  # immutable Prolly nodes addressed by SHA-256
```

Rows are ordered by a canonical primary-key encoding. Content-defined chunk
boundaries make roots deterministic and keep most chunks stable when nearby
rows change. The manifest is the small mutable pointer Git compares between
snapshots; object files are immutable and naturally deduplicate in Git packs.

This intentionally starts with tracked files instead of custom Git refs or
unreachable Git objects. It makes normal clone, branch, diff, and garbage
collection behavior predictable. A pack-aware object backend can be added after
the SQL/storage semantics are stable.

## Build sequence

1. **Persistent table adapter** — implement `sql.Table`, partitions, row
   iterators, inserters, updaters, and deleters over Prolly roots. Persist schema
   and primary-key metadata in the manifest.
2. **Transactions** — give each SQL session a working manifest; commit with an
   atomic compare-and-swap of the repository head. Readers retain immutable
   roots, providing snapshot isolation.
3. **Secondary indexes** — use one Prolly tree per index, keyed by encoded index
   columns plus the primary key. Update table and index roots atomically.
4. **Git-aware operations** — expose `REPO_COMMIT`, branch, log, diff, and merge
   as CLI commands and system tables. Merge trees by key using the common
   ancestor, surfacing row/schema conflicts explicitly.
5. **Production hardening** — locking, crash recovery, object GC, bounded caches,
   authentication/TLS, compatibility suites, fuzzing, and migration tooling.

The key rule is that Git commits database snapshots; SQL transactions should
not secretly create Git commits. This keeps high-frequency database writes
cheap while leaving history creation explicit and understandable.
