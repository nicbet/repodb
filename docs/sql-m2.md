# M2 persistent SQL contract

M2 provides one repository-backed SQL engine through two entry points: the Go
`engine` package and the MySQL wire server. Both use the same catalog, table,
transaction, and publication implementations. Embedded use opens no listener.

## Embedded API

```go
eng, err := engine.Open(ctx, repositoryPath)
session, err := eng.NewSession()
err = session.Exec(ctx,
    "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)")
err = session.Exec(ctx, "INSERT INTO issues VALUES (?, ?)", 1, "first")
result, err := session.Query(ctx, "SELECT * FROM issues WHERE id = ?", 1)
```

`Session.Begin` returns a transaction with `Exec`, `Query`, `Commit`, and
`Rollback`. Sessions serialize their own calls. Applications should create a
session per concurrent unit of work and close sessions and engines when done.

The initial parameter API accepts nil, booleans, integer and floating-point Go
values, strings, and byte slices. Values are rendered as SQL literals before
analysis; quotes and binary data are escaped by the engine.

## Initial SQL scope

M2 supports one database namespace, explicit primary keys, `CREATE TABLE`,
`DROP TABLE`, `INSERT`, `UPDATE`, `DELETE`, and ordinary `SELECT` expressions,
filters, projections, ordering, and limits handled by `go-mysql-server`.
Supported persisted column families are signed and unsigned integers, floating
point numbers, `CHAR`, `VARCHAR`, `TEXT`, `BINARY`, `VARBINARY`, and `BLOB`.

Every persisted table must declare a primary key. Auto-increment, defaults,
generated columns, secondary indexes, foreign keys, `ALTER TABLE`, savepoints,
and additional database namespaces are outside the M2 compatibility claim.
Unsupported schema features return an error even when the SQL parser accepts
their syntax.

## Transactions and isolation

A transaction pins `refs/repodb/data` at its first catalog access. Readers see a
stable snapshot plus their own writes. Each successful autocommit mutation or
explicit commit creates one data commit. Read-only and unchanged transactions
do not publish. Rollback publishes nothing.

This is snapshot isolation with optimistic, repository-wide publication. If the
data ref changed since the transaction began, commit returns
`repository.ErrConflict`; RepoDB does not replay application statements. A
failed statement restores that statement's table edits and leaves an explicit
transaction active. MySQL DDL has implicit-commit behavior: successful DDL is
durable even if a following `ROLLBACK` is issued.

Rows and composite primary keys use deterministic typed encodings. A commit
bulk-rebuilds each table's Prolly tree, validates every descendant, and derives
the snapshot inventory from schema roots and reachable Prolly nodes. Obsolete
chunks are excluded from the new snapshot; history keeps older snapshots alive.

## Publication outcomes

`Writer.CommitWithOutcome` returns one of three outcomes:

- `OutcomeRejected`: the live ref did not advance. Stale-head rejection is
  `repository.ErrConflict`; lock cancellation, permissions, and filesystem
  failures retain their original error class.
- `OutcomeCommitted`: the ref advanced to the returned candidate. An error with
  this outcome means publication succeeded but a later check failed and must
  not be treated as rollback.
- `OutcomeUnknown`: RepoDB could not establish whether the candidate became
  live. The error wraps `repository.ErrCommitUnknown`.

Call `Repository.RecoverCommit(ctx, candidate)` to inspect an uncertain
candidate. Do not replay the SQL transaction merely because commit returned an
error. Fault tests cover failures before, at, and after ref publication. A
separate-process test covers two writers in linked worktrees, and a killed
pre-publication writer test verifies that the old snapshot remains complete and
the operating system releases the publication lock.

Git update errors are classified as stale conflicts only after RepoDB reads the
live ref and observes a different head. A generic Git “cannot lock ref” message
is not itself considered a retryable conflict.

Embedded SQL commits preserve `*repository.CommitError` fields. Across MySQL,
the server emits a stable outcome and candidate marker; the Go wire client parses
it as `*client.CommitError`. Wire callers can then call
`Client.RecoverCommit`, backed by `repodb_recover_commit(candidate)`, to resolve
the candidate without filesystem access or statement replay.

## Bounded initial workload and measurement

The M2 implementation targets small tool databases: up to roughly 1,000 rows
per table, modest row values, and tens of writes per interactive operation. It
uses full-table rebuilds and full snapshot validation at transaction boundaries.
This is a correctness baseline, not the intended large-table scaling design.

Run `make m2-bench` to recreate `/tmp/repodb-m2` and measure open, begin, bulk
commit, repeated-update time, heap, live objects, and Git subprocess count. On a
2026-09-11 macOS development run with Git 2.55, 100 rows and ten autocommit
updates measured approximately 68 ms open, 60 ms begin, 156 ms bulk commit,
1.63 seconds for all updates, 4.6 MB heap, four live objects, and 199 Git
subprocesses. Snapshot object reads are batched and unchanged Git blob IDs are
reused, but process startup remains a visible cost. M3 or a later performance
pass should replace more per-operation CLI plumbing before increasing the
supported workload.
