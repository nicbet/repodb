# RepoDB

RepoDB is an embedded SQL database for Go that stores and synchronizes application data in Git, with an optional MySQL-compatible server.

Build issue trackers, kanban boards, and agent tools whose data travels with a
repository. Write data locally, work offline, and synchronize across clones.
Database transactions persist automatically, leaving your source files, index,
and code branches untouched. The default currently records each transaction in
Git data history; M4.3 also includes an opt-in durable-journal prototype for
intentional data commits.

**Early development:** persistent SQL, synchronization, and three-way merging are
implemented. The current scope is small tool databases with a documented
[SQL subset](docs/sql-m2.md); broader compatibility and scaling are on the roadmap.

## Quickstart

After [installing RepoDB](#installation), create a local demo repository and start
the server:

```sh
git init repodb-demo
cd repodb-demo
repodb init
repodb start
```

In a second terminal, create a table and query it using the included client:

```sh
repodb sql 'CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)'
repodb sql "INSERT INTO issues VALUES (1, 'Ship the first version')"
repodb sql 'SELECT * FROM issues'
```

Writes are persisted automatically. Stop and restart the server to read the same
data. The server listens on `127.0.0.1:3306` by default; you can also connect with
a MySQL client:

```sh
mysql --host=127.0.0.1 --port=3306 --user=root repodb
```

### Synchronize an existing project

From a Git repository with a configured remote:

```sh
repodb enable --remote origin
repodb sync --remote origin
```

`enable` adopts existing remote database history or initializes an empty catalog
if neither side has one. It is safe to repeat and does not start a server.
On a fresh clone, run `enable` before creating a separate local database with
`init`.

`sync` fetches and publishes database changes, merging independent row edits.
Competing edits are preserved for explicit resolution:

```sh
repodb conflicts --remote origin
repodb resolve --remote origin --id '<conflict-id>' --take local
```

Resolution choices are `local`, `remote`, `base`, and `delete`. See the
[merge guide](docs/merge-m4.md) for row and schema conflict behavior.

**Use `repodb sync` to share database changes.** Ordinary `git push` publishes
source branches according to your Git configuration. A successful SQL write is
durable locally and does not imply that the remote has received it.

## Installation

Build from source with **Go 1.27 or newer**, Git, and Make. The current
implementation requires POSIX file locking; the documented baseline uses macOS
and Git 2.55. See [storage and durability assumptions](docs/storage-format.md).

```sh
git clone https://github.com/nicbet/repodb.git
cd repodb
make build
export PATH="$PWD/bin:$PATH"
```

This builds `bin/repodb` and `bin/repodb-server` and adds them to the current
shell's `PATH`. Add the absolute `bin` path to your shell configuration to keep
them available in new terminals.

For embedded Go applications, add the module to your project:

```sh
go get github.com/nicbet/repodb/engine
```

Import `github.com/nicbet/repodb/engine`, open an initialized repository with
`engine.Open`, and create a session with `NewSession`. Embedded applications own
the engine lifecycle and need no server or network listener. See the
[embedded API and transaction guide](docs/sql-m2.md).

You can also install the command-line programs directly:

```sh
go install github.com/nicbet/repodb/cmd/repodb@latest
go install github.com/nicbet/repodb/cmd/repodb-server@latest
```

## Development

```sh
make build
make test
go test -race ./common/repository ./engine ./integration
git diff --check
```

The test suite covers persistent SQL, embedded/MySQL interoperability, transaction
outcome recovery, concurrent writers, Git transport, and merge conflicts.

Run the [database scorecard](docs/benchmark.md) for a consolidated report from the
current checkout. Both persistence modes execute the same workloads:

```sh
make bench                        # Native Git persistence
make bench BENCH_MODE=journal      # Journal persistence
```

Additional experiments support focused storage and performance work:

```sh
make m0        # Git storage and transport experiment
make m2-bench  # Bounded SQL persistence benchmark
make m4.3-bench # Paired native-Git/journal persistence benchmark
```

The first two recreate `/tmp/repodb-m0` and `/tmp/repodb-m2`; the M4.3 target
runs paired Go benchmarks. The [SQL guide](docs/sql-m2.md) records workload
limits and [the M4.3 report](docs/m4.3-bench.md) records the new comparison.

## Design and Architecture

RepoDB combines `go-mysql-server` for SQL execution, DoltHub's Vitess fork for
MySQL parsing and protocol support, immutable Prolly trees for table data, and
Git objects and refs for persistence and transport.

```text
Go application          MySQL client
      |                       |
      |                  server/
      +-----------+-----------+
                  |
               engine/
        SQL tables and transactions
                  |
        common/prolly/ + common/repository/
        Immutable data and Git snapshots
                  |
         refs/repodb/data
                  |
             integration/
         Sync and reconciliation
                  |
              Git remote
```

- **One engine, two entry points.** Embedded sessions and MySQL connections use
  the same catalog, table adapters, and transaction implementation.
- **Independent database history.** Each write transaction that changes state
  publishes a data commit under `refs/repodb/data`. Git trees reference every
  required schema and data blob so snapshots survive transport and garbage
  collection.
- **Snapshot isolation.** Transactions read a pinned snapshot plus their own
  writes. Publication uses a repository-wide lock and an expected-head check;
  stale writers receive a conflict instead of overwriting newer data.
- **Explicit synchronization.** Remote data is fetched into separate tracking
  refs. Sync validates incoming state, fast-forwards compatible histories, and
  uses a three-way merge for divergence. Conflicts remain inspectable across
  restarts.

The current SQL scope supports one database namespace, explicit primary keys,
basic DDL/DML, and a limited set of persisted types. Secondary indexes,
auto-increment, foreign keys, and `ALTER TABLE` are not yet supported. Table
updates use bulk rebuilds within the documented small-database workload.

| Documentation | Covers |
| --- | --- |
| [SQL and embedded API](docs/sql-m2.md) | Supported types, transactions, commit recovery, and workload bounds |
| [Storage format](docs/storage-format.md) | Snapshots, object inventories, locking, durability, and legacy import |
| [Git integration](docs/git-integration.md) | Ref layout and ordinary Git command behavior |
| [Synchronization](docs/sync-m3.md) | Enable, tracking refs, and transport |
| [Merging](docs/merge-m4.md) | Three-way merge, conflict resolution, and distributed row identity |
| [Working state prototype](docs/working-state.md) | Durable journal, checkpoints, limitations, and adoption decision |

## Roadmap

- [x] **M0–M1:** Git storage experiments and durable repository snapshots.
- [x] **M2:** Persistent embedded SQL and a shared MySQL server.
- [x] **M3:** Repository enablement and explicit synchronization.
- [x] **M4:** Three-way merging and persistent conflict resolution.
- [ ] **M5:** Example applications and broader integration coverage.
- [ ] **M6:** Broader SQL compatibility, indexing, performance, and operational hardening.

See [the implementation plan](docs/plan.md) for milestone scope and acceptance criteria.

## Contributing

Issues and pull requests are welcome. For substantial changes, open an issue to
discuss the use case and approach first; the [implementation plan](docs/plan.md) is the
starting point for scope and priorities.

Keep pull requests focused, format changed Go files with `gofmt`, and add tests
for behavioral changes. Run the development checks above and update the relevant
documentation when changing SQL behavior, storage, or synchronization. Bug reports
should include a minimal reproduction, the operating system, Go and Git versions,
and the expected and actual behavior.
