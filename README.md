# RepoDB

RepoDB is an experiment in shared database infrastructure for tools whose state
belongs to an existing Git repository. The target is an embedded Go engine with
an optional MySQL-compatible server, automatic persistence under dedicated Git
refs, and synchronization configured through `repodb enable`.

Database history is independent of code history. Applications control the
embedded engine's lifecycle; `repodb start` offers a separate server when
wanted. Enabling repository integration will not start a server.

See [plan.md](plan.md) for the agreed requirements, proposed architecture, and
implementation milestones. The enable/sync/start workflow is implemented.
The M0 storage and ordinary-Git command contract is recorded in
[docs/git-integration.md](docs/git-integration.md).

The current prototype is written in Go and combines:

- `go-mysql-server` for SQL analysis, planning, and execution;
- DoltHub's Vitess fork for MySQL syntax and the wire protocol;
- an immutable, content-addressed Prolly tree for table data; and
- ordinary Git commits for history, branching, transport, and collaboration.

## Status

M2 provides persistent SQL through an embedded Go engine and the MySQL server.
Schemas and typed rows are stored in deterministic Prolly trees and every
successful write transaction automatically publishes one independent Git data
commit under `refs/repodb/data`. See [the M2 SQL contract](docs/sql-m2.md) for
the supported SQL scope, transaction behavior, outcome recovery, and measured
initial workload.

M3 adds explicit Git transport. `repodb enable --remote <name>` configures and
fetches a separate tracking ref, and `repodb sync --remote <name>` performs only
validated fast-forwards. Divergence preserves both histories for M4. See
[the M3 synchronization contract](docs/sync-m3.md).

## Layout

```text
cmd/repodb/          CLI: initialize, inspect, snapshot, and query
cmd/repodb-server/   MySQL-compatible server process
client/              reusable MySQL wire client
server/              go-mysql-server host and storage adapters
engine/              embedded persistent SQL engine and table adapters
integration/         enable and explicit fast-forward synchronization
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
./bin/repodb enable --remote origin
./bin/repodb sync --remote origin
./bin/repodb start -repo .
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
or branch. SQL transactions publish this separate history automatically.

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
and the server, add enable/sync, then implement distributed merge and compatibility work.
Ordinary writes will persist automatically without user-managed Git snapshots.
