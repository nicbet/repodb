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
The M0 storage and ordinary-Git command contract is recorded in
[docs/git-integration.md](docs/git-integration.md).

The current prototype is written in Go and combines:

- `go-mysql-server` for SQL analysis, planning, and execution;
- DoltHub's Vitess fork for MySQL syntax and the wire protocol;
- an immutable, content-addressed Prolly tree for table data; and
- ordinary Git commits for history, branching, transport, and collaboration.

## Status

This repository contains the first executable vertical slice. The MySQL server
accepts DDL and DML, the client speaks the MySQL protocol, and the common module
can build and read deterministic Prolly trees. M1 adds durable catalog snapshots
stored entirely in Git objects under `refs/repodb/data`.

SQL tables currently use the `go-mysql-server` in-memory adapter. They are **not
yet persisted** to Prolly roots. The repository snapshot API is ready for that
integration in M2.

## Layout

```text
cmd/repodb/          CLI: initialize, inspect, snapshot, and query
cmd/repodb-server/   MySQL-compatible server process
client/              reusable MySQL wire client
server/              go-mysql-server host and storage adapters
common/prolly/       deterministic content-defined tree construction
common/storage/      content-addressed memory and filesystem stores
common/repository/   durable Git snapshots, manifests, and legacy import
common/git/          typed Git object/ref plumbing boundary
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
make m0 # repeat the Git integration experiment under /tmp/repodb-m0
./bin/repodb init .
./bin/repodb-server -repo .
```

In a second terminal, either use any MySQL client or the included one:

```sh
./bin/repodb sql 'SELECT 1 + 1 AS answer'
mysql --host=127.0.0.1 --port=3306 --user=root repodb
```

Repository state now lives outside the source branch:

```sh
./bin/repodb status
./bin/repodb import-legacy # only for the superseded tracked .repodb layout
```

Snapshot publication never stages files or changes the source worktree, index,
or branch. SQL transaction publication will be connected in M2.

## Git snapshot model

```text
refs/repodb/data -> commit -> tree
  manifest.json
  objects/sha256/ab/cdef...  # immutable objects addressed by RepoDB SHA-256
```

See [docs/storage-format.md](docs/storage-format.md) for format, integrity,
concurrency, cache, durability, and legacy migration details.

## Build sequence

Follow the milestones and acceptance criteria in [plan.md](plan.md): prove Git
storage/transport, implement durable snapshots, connect persistent embedded SQL
and the server, then add enable/sync, distributed merge, and compatibility work.
Ordinary writes will persist automatically without user-managed Git snapshots.
