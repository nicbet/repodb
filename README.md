# RepoDB

RepoDB is an experiment in shared database infrastructure for tools whose state
belongs to an existing Git repository. The target is an embedded Go engine with
an optional MySQL-compatible server, automatic persistence under dedicated Git
refs, and synchronization configured through `repodb enable`.

Database history is independent of code history. Applications control the
embedded engine's lifecycle; `repodb start` will offer a separate server when
wanted. Enabling repository integration will not start a server.

See [plan.md](plan.md) for the agreed requirements, proposed architecture, and
implementation milestones. The enable/start workflow is planned, not implemented.

The current prototype is written in Go and combines:

- `go-mysql-server` for SQL analysis, planning, and execution;
- DoltHub's Vitess fork for MySQL syntax and the wire protocol;
- an immutable, content-addressed Prolly tree for table data; and
- ordinary Git commits for history, branching, transport, and collaboration.

## Status

This repository contains the first executable vertical slice. The MySQL server
accepts DDL and DML, the client speaks the MySQL protocol, and the common module
can build and read deterministic Prolly trees persisted beneath `.repodb/`.

SQL tables currently use the `go-mysql-server` in-memory adapter. They are **not
yet persisted** to Prolly roots. The current tracked `.repodb/` layout and manual
snapshot commands predate the dedicated-ref design in the plan.

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

The prototype also exposes these commands for its tracked `.repodb/` files;
they do not currently snapshot SQL tables:

```sh
./bin/repodb status
./bin/repodb snapshot -m 'Update application data'
```

RepoDB never commits unrelated worktree changes: snapshot commands restrict Git
operations to `.repodb`.

## Prototype on-disk model

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

This layout will be replaced by reachable Git objects under dedicated RepoDB
refs. See the plan for reachability, automatic transaction publication, and
cache-rebuild requirements.

## Build sequence

Follow the milestones and acceptance criteria in [plan.md](plan.md): prove Git
storage/transport, implement durable snapshots, connect persistent embedded SQL
and the server, then add enable/sync, distributed merge, and compatibility work.
Ordinary writes will persist automatically without user-managed Git snapshots.
