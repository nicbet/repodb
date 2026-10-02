# RepoDB

![RepoDB Banner](docs/banner.png)

RepoDB is an embedded SQL database for Go that stores and synchronizes application data in Git, with an optional MySQL-compatible server.

Build agent tools, issue trackers and dashboards whose data travels with a repository. Write locally, work offline, and synchronize across clones. SQL transactions are durable as soon as they commit; you choose when to checkpoint them into Git history. RepoDB keeps its data on its own Git ref and never touches your source files, index or branches.

**Early development.** Persistent SQL, synchronization and three-way merging are implemented, tested and benchmarked, for local-first, version-controlled relational workloads within a documented [SQL subset](docs/sql.md). At the sizes measured so far (up to 50k rows) RepoDB serves point and indexed-range reads in well under a millisecond, a 50k-row full scan in about 13 ms, single-row writes in 0.3–0.4 ms and 100-row batches in about 3 ms (journal mode), and cross-clone merges in about half a second; see the [latest benchmark results](docs/benchmarks/latest.md) against MySQL 8.4 and Dolt. Concurrent writers are the main limitation: a write transaction is rejected whenever another commits first. The storage format still changes between versions without migration, so treat stored data as disposable for now.

## Quickstart

After [installing RepoDB](#installation), create a demo repository and start the server:

```sh
# Create a demo directory and initialize Git
mkdir repodb-demo
cd repodb-demo
git init

# Initialize RepoDB's data ref
repodb init

# Start the SQL server
repodb start
```

The server listens on `127.0.0.1:3306` by default, and any MySQL client can connect:

```sh
mysql --host=127.0.0.1 --port=3306 --user=root repodb
```

Writes persist immediately: stop and restart the server, and the data is still there.

You can also run statements with `repodb sql`. In a second terminal, create a table and query it:

```sh
repodb sql 'CREATE TABLE greetings (id BIGINT PRIMARY KEY, title TEXT NOT NULL)'
repodb sql "INSERT INTO greetings VALUES (1, 'Hello from RepoDB!')"
repodb sql 'SELECT * FROM greetings'
```

### Synchronize an existing project

From a Git repository with a configured remote:

```sh
repodb enable --remote origin
repodb sync
```

`enable` adopts the remote's database history, or initializes an empty catalog if neither side has one. It is safe to repeat and does not start a server. On a fresh clone, run `enable` instead of `init`, so you don't start a separate local database.

`sync` fetches and publishes database changes, merging independent row edits. Writes first land in a local journal: `repodb sync --commit -m '<message>'` checkpoints them into a data commit and then syncs, and in a terminal plain `repodb sync` offers to do the same. Competing edits to the same row are kept for explicit resolution:

```sh
repodb conflicts
repodb resolve --id '<conflict-id>' --take local
```

Resolution choices are `local`, `remote`, `base` and `delete`. See [cli.md](docs/cli.md#synchronizing-with-a-remote) for row and schema conflicts.

After `enable`, every sync command defaults to the configured remote; pass `--remote` only to override it.

**Use `repodb sync` to share database changes.** `git push` publishes your source branches as usual, but not the data ref. A successful SQL write is durable locally; it does not mean the remote has it.

## CLI commands

| Command                       | Description                                                                 |
| ----------------------------- | --------------------------------------------------------------------------- |
| `repodb init [path]`          | Initialize a RepoDB data ref in a Git repository                            |
| `repodb start`                | Start a MySQL-compatible server (`--addr`, `--persistence`, `--durability`) |
| `repodb sql '<statement>'`    | Run a statement against a running server (`--addr`, `--database`, `--user`) |
| `repodb status`               | Show the data head, format version, object and table counts, working state  |
| `repodb diff`                 | List tables with uncommitted (not yet checkpointed) changes                 |
| `repodb commit -m '<msg>'`    | Checkpoint the journal into a Git data commit                               |
| `repodb enable`               | Set up sync with a remote (`--remote`); safe to repeat                      |
| `repodb sync`                 | Fetch and publish data history (`--commit -m` checkpoints first)            |
| `repodb conflicts`            | List unresolved merge conflicts after a sync                                |
| `repodb resolve`              | Resolve a conflict (`--id`, `--take local\|remote\|base\|delete`)           |

## Embedded use

Go applications can use the engine directly, without a server:

```sh
go get github.com/nicbet/repodb/engine
```

Build with `-tags gms_pure_go` (for example `go build -tags gms_pure_go ./...`, or set `GOFLAGS=-tags=gms_pure_go`). RepoDB runs SQL through go-mysql-server, whose default regular-expression backend links the ICU4C C library through cgo; the tag selects its pure-Go backend, so SQL `REGEXP` functions use Go's RE2 syntax. Without the tag you need cgo, a C++ toolchain and ICU4C.

```go
eng, err := engine.Open(ctx, ".")
if err != nil {
	return err
}
defer eng.Close()

session, _ := eng.NewSession()
defer session.Close()

session.Exec(ctx, "CREATE TABLE events (id BIGINT PRIMARY KEY, data TEXT NOT NULL)")
session.Exec(ctx, "INSERT INTO events VALUES (?, ?)", 1, payload)

result, _ := session.Query(ctx, "SELECT data FROM events WHERE id = ?", 1)
```

Like the CLI and the server, the engine defaults to journal persistence: each SQL commit is durable in a local journal, without a Git commit per transaction. Call `eng.Checkpoint` to publish the accumulated changes to Git history, and `integration.Sync` to exchange them with a remote. The [Go library guide](docs/library.md) covers transactions, commit outcomes and the sync API.

## Installation

Build from source with **Go 1.26 or newer**, Git and Make:

```sh
git clone https://github.com/nicbet/repodb.git
cd repodb
make build
export PATH="$PWD/bin:$PATH"
```

Or install the command-line programs directly:

```sh
go install -tags gms_pure_go github.com/nicbet/repodb/cmd/repodb@latest
go install -tags gms_pure_go github.com/nicbet/repodb/cmd/repodb-server@latest
```

The test suite runs on macOS (Git 2.55). Linux shares the Unix code paths and runs the benchmark harness, but has no separate test run. Windows builds and passes `go vet` in every `make test`, but its test suite has not been run yet. See [platforms](docs/architecture.md#platforms).

## Performance

Measured 2026-10-02 at `6b76e87`, p50 at 50k rows, with **every system in a Linux container on the same Docker VM** (an Apple M1 Max host) at its default durability. RepoDB runs embedded in the benchmark harness; MySQL and Dolt are reached over TCP between containers. See the [latest results](docs/benchmarks/latest.md) for every size, concurrency, sync and the environment, and the [setup](docs/benchmark.md#setup) for exactly what is compared.

Journal-mode writes are faster than MySQL's for single rows (0.34–0.40 ms against 0.53–0.69 ms) and about 3× faster for 100-row batches, because the journal appends one record per transaction. Point reads, ranges, short read transactions, ordered limits and full scans are on par with MySQL or faster. Joins are slower: go-mysql-server, which runs RepoDB's SQL, joins and groups row by row, where MySQL resolves this join to a constant and counts.

| Workload (50k rows) | MySQL 8.4 | Dolt 2.3 | RepoDB Journal |
| ------------------- | --------: | -------: | -------------: |
| Point read          |   0.04 ms |  0.16 ms |        0.04 ms |
| Range (100 rows)    |   0.10 ms |  0.26 ms |        0.07 ms |
| Read tx (10 reads)  |   0.68 ms |   2.2 ms |        0.26 ms |
| Ordered limit, DESC |   0.04 ms |  0.21 ms |        0.04 ms |
| Full scan           |     13 ms |    21 ms |          13 ms |
| Join + aggregate    |    2.6 ms |    12 ms |          15 ms |
| Update x1           |   0.69 ms |   1.0 ms |        0.34 ms |
| Insert              |   0.54 ms |  0.83 ms |        0.40 ms |
| Delete              |   0.53 ms |  0.80 ms |        0.38 ms |
| Update x100         |    8.7 ms |    26 ms |         2.8 ms |

Native-Git (audit) mode, which makes every transaction a Git commit, takes 14–17 ms per write at 50k rows. A write transaction is rejected rather than queued when another commits first, even if the two wrote different rows, so concurrent writers see conflicts that MySQL would not report. A divergent merge sync takes about 0.5 s at 50k rows in journal mode. Dolt also versions, clones, pushes and merges its data, including to Git remotes, but the harness runs sync and merge workloads only against RepoDB, so those results have no baseline.

## Development

```sh
make build
make test
make lint
go test -race -tags gms_pure_go ./common/repository ./engine ./integration
git diff --check
```

Run the [database scorecard](docs/benchmark.md) ([how results are published](docs/benchmark.md#publishing-results)):

```sh
make bench-docker BENCH_MODE=journal     # RepoDB in Docker, like the baselines
make bench-docker BENCH_MODE=native-git
make bench-docker BENCH_MODE=external BENCH_SUFFIX=-mysql \
  BENCH_DSN='root@tcp(repodb-bench-mysql:3306)/'   # MySQL or Dolt baseline
```

## Architecture

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
        Immutable data, journal, and Git snapshots
                  |
         refs/repodb/data
                  |
             integration/
         Sync and reconciliation
                  |
              Git remote
```

- **One engine, two entry points.** Embedded sessions and MySQL connections share the same catalog, table adapters and transactions.
- **Content-addressed tables.** Each table and secondary index is a Prolly tree, a B-tree whose node boundaries depend only on content, stored as Git blobs. Equal tables have equal roots, unchanged subtrees are shared between commits, and an update rewrites one leaf and its path to the root. Nodes are binary and read in place, and queries decode only the columns they use.
- **Journal persistence.** SQL commits append their row edits to a local journal and flush it. By default a commit survives process and OS crashes; `full` durability also survives power loss. A checkpoint applies the journal to the trees and publishes a Git data commit. Journal mode is the default for the library, the CLI and the server. Native-Git (audit) mode makes every transaction a Git data commit instead.
- **Snapshot isolation.** A transaction reads a pinned snapshot plus its own writes. A stale writer gets a conflict instead of overwriting newer data.
- **Explicit synchronization.** Remote data is fetched into a separate tracking ref. Sync validates it, then fast-forwards, pushes or three-way merges by primary key. Conflicts stay inspectable across restarts.

The SQL subset covers:
- **Storage:** one database; tables with explicit (possibly composite) primary keys; secondary indexes, unique and non-unique.
- **Statements:** DDL including `ALTER TABLE`, and DML including `INSERT … ON DUPLICATE KEY UPDATE`.
- **Constraints:** `CHECK` constraints, `DEFAULT` values, and collation-aware comparison and uniqueness.
- **Types:** signed and unsigned integers, `FLOAT`, `DOUBLE`, `DECIMAL`, `CHAR`, `VARCHAR`, `TEXT`, `BINARY`, `VARBINARY`, `BLOB`, `ENUM`, `JSON`, `DATE`, `TIME`, `DATETIME` and `TIMESTAMP`.

Not yet supported: `AUTO_INCREMENT` (rdb-a2d28e), foreign keys (rdb-48af2f) and views (rdb-5c1808). See the [SQL reference](docs/sql.md).

| Documentation                                | Covers                                                                          |
| -------------------------------------------- | ------------------------------------------------------------------------------- |
| [Command line](docs/cli.md)                  | Setup, server, persistence modes, checkpoints, sync, conflicts, backup          |
| [Go library](docs/library.md)                | Engine, sessions, transactions, commit outcomes, sync API, server and client    |
| [SQL reference](docs/sql.md)                 | Supported types, statements, indexes, transactions, and what is unsupported     |
| [Architecture](docs/architecture.md)         | Storage format, persistence modes, concurrency, sync and merge, durability      |
| [Testing](docs/testing.md)                   | Test suites and what they protect, fuzzing, benchmarks, and known gaps          |
| [Benchmarks](docs/benchmarks/latest.md)      | Latest results against MySQL and Dolt; [methodology](docs/benchmark.md)         |

## Roadmap

Done
- [x] Git-backed storage, persistent SQL, sync and three-way merging, journal persistence
- [x] CLI, embedded library, and MySQL-compatible server
- [x] Secondary indexes, `ALTER TABLE`, expanded types and constraints, collations
- [x] Indexed range and ordered-scan queries in both directions, compact binary rows and tree nodes
- [x] Fast scans, joins and counts: in-place node reads, column projection, stored row counts
- [x] Journal and index overlays whose cost does not grow with uncheckpointed edits

Now: finish the performance envelope
- [ ] Fewer Git subprocesses per sync (rdb-2c93ca); append-heavy and mutable-data workload profiling (rdb-db5459)
- [ ] Concurrent writers: per-key conflict detection, group commit, safe automatic retry, MySQL-compatible conflict errors (rdb-df092b)

Next: a standalone Git-native relational database
- [ ] Version-controlled data: snapshot addressing, history, historical queries, diffs, named branches, switching, merging, restore, revert
- [ ] Secure server: configuration and secrets, authentication and TLS, roles and privileges, observability
- [ ] Application compatibility: `AUTO_INCREMENT`, foreign keys, views, driver and ORM metadata, a published compatibility profile
- [ ] Operational durability: backup and restore, corruption recovery, journal retention, history retention, format migration

The roadmap is maintained as the project's issue backlog (see [Contributing](#contributing)).

## Contributing

Issues and pull requests are welcome. For substantial changes, open an issue to discuss the use case and approach first. [docs/architecture.md](docs/architecture.md) describes how the system works today.

Issues, including the roadmap, are tracked with [Exponential](https://go-exponential.dev) and travel with this repository (`.xpo/`).

Keep pull requests focused, format changed Go files with `gofmt`, and add tests for behavioral changes. Run the development checks above ([testing.md](docs/testing.md)). The documents in `docs/` describe current behavior only: a change that alters behavior updates them in the same change.
