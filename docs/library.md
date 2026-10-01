# Go library

A Go program can use RepoDB in-process: open a Git repository, run SQL, checkpoint, and synchronize, all without a server. This page covers the public packages:

| Package | Use |
| --- | --- |
| `github.com/nicbet/repodb/engine` | open a repository and run SQL: `Engine`, `Session`, `Tx` |
| `github.com/nicbet/repodb/common/repository` | repository lifecycle, snapshots, errors, commit outcomes, the journal (`WorkingState`) |
| `github.com/nicbet/repodb/integration` | `Enable`, `Sync`, `Conflicts`, `Resolve`: what the `repodb` sync commands call |
| `github.com/nicbet/repodb/server` | embed the MySQL wire server |
| `github.com/nicbet/repodb/client` | a small MySQL client that understands RepoDB commit errors |

The SQL these accept is described in [sql.md](sql.md), and the storage behind them in [architecture.md](architecture.md).

## Build requirements

```sh
go get github.com/nicbet/repodb
```

Build with `-tags gms_pure_go`, for example `go build -tags gms_pure_go ./...` or `GOFLAGS=-tags=gms_pure_go`. go-mysql-server's default regular-expression backend links ICU4C through cgo; the tag selects its pure-Go backend, so SQL `REGEXP` uses Go's RE2 syntax. Without the tag you need cgo, a C++ toolchain and ICU4C.

RepoDB runs the `git` executable. Git must be on `PATH`.

## Opening a repository

The directory must be inside a Git repository that already has RepoDB data. Create the data namespace once with `repository.Init` (or `repodb init`), or adopt a remote's history with `integration.Enable`.

```go
package main

import (
	"context"
	"errors"
	"log"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func main() {
	ctx := context.Background()
	if _, err := repository.Init(ctx, "."); err != nil {
		log.Println("init:", err) // fails if RepoDB data already exists
	}

	eng, err := engine.Open(ctx, ".") // journal persistence
	if errors.Is(err, repository.ErrNotInitialized) {
		log.Fatal("run repodb init or repodb enable first")
	}
	if err != nil {
		log.Fatal(err)
	}
	defer eng.Close()
}
```

`engine.Open` uses **journal** persistence: each committed transaction is appended to a local journal with one `fsync`, and reaches Git history when you call `Checkpoint`. To publish a Git data commit per transaction, choose native-git:

```go
eng, err := engine.OpenWithOptions(ctx, ".", engine.Options{
	Persistence: engine.PersistenceNativeGit,
})
```

`engine.New(repo)` and `engine.NewWithOptions(repo, options)` do the same for a `*repository.Repository` you already opened. The trade-offs between the two modes are in [cli.md](cli.md#persistence-modes).

A native-git engine refuses to open while the journal has uncheckpointed changes (`repository.ErrWorkingStateDirty`). Checkpoint in journal mode first. A native-git engine that is already open fails its commits with the same error while another engine's journal is dirty, and nothing is written.

Within a process, share one `Engine` per repository. Separate engines, whether in this process, other processes or a server, can use the same repository at the same time: every transaction boundary re-checks the journal and the data ref, so each sees the others' commits.

## Sessions and queries

A `Session` is one connection's worth of state. Calls on a session are serialized, so give each goroutine or unit of work its own session.

```go
session, err := eng.NewSession()
if err != nil {
	log.Fatal(err)
}
defer session.Close()

if err := session.Exec(ctx, "CREATE TABLE events (id BIGINT PRIMARY KEY, body TEXT NOT NULL)"); err != nil {
	log.Fatal(err)
}
if err := session.Exec(ctx, "INSERT INTO events VALUES (?, ?)", 1, "hello"); err != nil {
	log.Fatal(err)
}
result, err := session.Query(ctx, "SELECT id, body FROM events WHERE id = ?", 1)
if err != nil {
	log.Fatal(err)
}
for _, row := range result.Rows {
	log.Println(row[0].(int64), row[1].(string))
}
```

- **Autocommit.** Outside an explicit transaction, each `Exec` or `Query` is its own transaction.
- **Results.** `Query` returns an `engine.Result` with `Columns []string` and `Rows [][]any`. All rows are collected before it returns.
- **Value types.** Values have go-mysql-server's Go types for the column: `int8`…`int64` and `uint8`…`uint64` by integer width, `float32`/`float64`, `string`, `[]byte`, `time.Time` (UTC), `*apd.Decimal`, JSON documents, and `nil` for NULL. `ENUM` values come back as strings.
- **Parameters.** Each `?` is replaced by an escaped SQL literal before parsing. Accepted parameter types:
  - `nil`, `bool`;
  - all Go integer and float types;
  - `string`, `[]byte`;
  - `time.Time` (converted to UTC);
  - `decimal.Decimal` (shopspring) and `*apd.Decimal`.

  Any other type fails with `unsupported SQL parameter type`. A `?` inside a quoted string is not a parameter.

## Transactions

```go
tx, err := session.Begin(ctx)
if err != nil {
	log.Fatal(err)
}
if err := tx.Exec(ctx, "UPDATE events SET body = ? WHERE id = ?", "edited", 1); err != nil {
	_ = tx.Rollback(ctx)
	log.Fatal(err)
}
if err := tx.Commit(ctx); errors.Is(err, repository.ErrConflict) {
	// Another transaction committed first. Nothing was written: retry the whole transaction.
} else if err != nil {
	log.Fatal(err)
}
```

`Tx` has `Exec`, `Query`, `Commit` and `Rollback`.

**Snapshot isolation.** A transaction reads the snapshot taken at its first table access, plus its own writes.

**Conflicts are repository-wide.** `Commit` returns `repository.ErrConflict` if any other transaction committed after that snapshot, even one that wrote different rows. RepoDB never re-runs your statements, so retry in application code (rdb-df092b tracks narrower conflicts and automatic retry).

A failed statement undoes only its own changes and leaves the transaction open. DDL commits implicitly, as in MySQL.

### Commit outcomes

Most commit errors mean nothing was written. Two error types carry an explicit outcome instead:

- **`*repository.CommitError`** (native-git publication) has `Outcome` (`OutcomeRejected`, `OutcomeCommitted`, `OutcomeUnknown`) and `Commit`, the candidate commit ID.
  - `OutcomeCommitted` with an error means the data commit was published but a later check failed. Don't treat it as a rollback.
  - `OutcomeUnknown` (wrapping `repository.ErrCommitUnknown`) means RepoDB could not tell. Resolve it with `Repository.RecoverCommit(ctx, candidate)` or SQL `repodb_recover_commit('<candidate>')`.
- **`*repository.WorkingCommitError`** (journal append) has `Outcome` and `TransactionID`. Resolve an unknown outcome with `WorkingState.RecoverTransaction(ctx, transactionID)`, preferably before the next checkpoint. Each checkpoint compacts the journal; transactions more than one checkpoint old come back as `unknown` with `repository.ErrWorkingHistoryTruncated`.

```go
var commitErr *repository.CommitError
if errors.As(err, &commitErr) && commitErr.Outcome == repository.OutcomeUnknown {
	resolved, recoverErr := eng.Repository().RecoverCommit(ctx, commitErr.Commit)
	if recoverErr != nil {
		log.Fatal(recoverErr)
	}
	log.Println("commit", commitErr.Commit, "was", resolved.Outcome)
}
```

Never re-run a transaction just because its commit returned an error: check the outcome first.

## Checkpoints and working state

In journal mode, `Checkpoint` turns everything since the last checkpoint into one Git data commit:

```go
result, err := eng.Checkpoint(ctx, "import customer list")
if err != nil {
	log.Fatal(err)
}
log.Println("data commit", result.Commit)
```

With nothing to checkpoint, it returns the current data head and creates no commit. In native-git mode `Checkpoint` returns an error, because every transaction is already a commit.

`eng.WorkingState()` exposes the journal:
- `Status(ctx)` returns the base and head commits, the generation, and `Dirty`;
- `Diff(ctx)` lists tables changed since the last checkpoint as `TableChange{Table, Change}`, with `added`, `modified` or `deleted`. Row-only changes count as `modified`.

It is `nil` in native-git mode. Use `repository.OpenWorkingState(repo)` to inspect a repository's journal without an engine.

## Synchronization

The `integration` package implements the sync workflow described in [cli.md](cli.md#synchronizing-with-a-remote). Each function takes a path inside the repository and a remote name:

```go
status, err := integration.Enable(ctx, ".", "origin")          // remote name required
status, err = integration.Sync(ctx, ".", "")                    // "" uses repodb.remote from enable
var conflictErr *integration.MergeConflictError
if errors.As(err, &conflictErr) {
	for _, c := range conflictErr.Set.Unresolved() {
		log.Println(c.ID, c.Kind, c.Table)
		_, err = integration.Resolve(ctx, ".", "", c.ID, engine.TakeRemote)
	}
}
log.Println(status.Action, status.LocalHead, status.RemoteHead)
```

- **`Status`** reports `Remote`, `TrackingRef`, `LocalHead`, `RemoteHead` and `Action` (for example `up-to-date`, `pushed`, `merged`, `conflicts`).
- **`Sync` errors:**
  - `integration.ErrWorkingDirty` when the journal has uncheckpointed changes: call `Checkpoint` first, or use `SyncWithOptions` (below);
  - `integration.ErrNotEnabled` before `Enable`;
  - `integration.ErrRemoteRequired` when no remote is given or configured;
  - `*integration.MergeConflictError` when the merge stopped on conflicts. `integration.Conflicts` reloads the saved conflict set later.
- **`Resolve`** takes `engine.TakeLocal`, `TakeRemote`, `TakeBase` or `TakeDelete`. It returns a `*MergeConflictError` while conflicts remain, and publishes and pushes the merge once none do.

If a journal transaction commits while `Sync` runs, `Sync` leaves the local data ref unchanged and returns `integration.ErrWorkingDirty`: checkpoint and call `Sync` again.

`integration.SyncWithOptions(ctx, path, remote, integration.SyncOptions{Checkpoint: f})` does that itself. Before each attempt it calls `f` if the journal is dirty, and a transaction that commits during the attempt makes it retry instead of fail, up to three attempts. `f` usually wraps `eng.Checkpoint`:

```go
status, err := integration.SyncWithOptions(ctx, ".", "origin", integration.SyncOptions{
	Checkpoint: func(ctx context.Context) error {
		_, err := eng.Checkpoint(ctx, "checkpoint before sync")
		return err
	},
})
```

## Embedding the MySQL server

```go
repo, err := repository.Open(ctx, ".")
if err != nil {
	log.Fatal(err)
}
srv, err := server.New(server.Config{
	Address:     "127.0.0.1:3306",
	Repository:  repo,
	Persistence: engine.PersistenceJournal,
})
if err != nil {
	log.Fatal(err)
}
log.Println("listening on", srv.Address())
if err := srv.Serve(ctx); err != nil { // returns when ctx is cancelled
	log.Fatal(err)
}
```

- **Lifecycle.** `Start` serves in the calling goroutine until `Close`, and `Serve(ctx)` wraps `Start` and `Close` around a context.
- **Defaults.** `Config.Persistence` defaults to journal, like the library and `repodb start`. The server exposes one database, named by the repository's manifest (`repodb`).
- **Security.** The server has no authentication or TLS. See [sql.md](sql.md#server-access).

## Client

`client` wraps `go-sql-driver/mysql` for talking to a RepoDB server:

```go
cli, err := client.Open(client.Config{Address: "127.0.0.1:3306", Database: "repodb"})
if err != nil {
	log.Fatal(err)
}
defer cli.Close()
rows, err := cli.Query(ctx, "SELECT id, body FROM events")
var commitErr *client.CommitError
if errors.As(err, &commitErr) && commitErr.Outcome == repository.OutcomeUnknown {
	outcome, _ := cli.RecoverCommit(ctx, commitErr.Candidate)
	log.Println("commit was", outcome)
}
log.Println(rows.Columns, len(rows.Rows))
```

- **Methods.** `Exec` returns a `database/sql` `sql.Result`, and `Query` returns `client.Result` with `Rows [][]*string` (`nil` for NULL).
- **Commit errors.** The server encodes commit outcomes in its error text; the client turns them back into `*client.CommitError` (`Outcome`, `Candidate`), and `RecoverCommit` asks the server to resolve the candidate.
- **Other clients.** Any MySQL client can connect. They see commit errors as plain MySQL errors.
