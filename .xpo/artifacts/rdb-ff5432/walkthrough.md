# rdb-ff5432 walkthrough: writes inside READ ONLY transactions

## The bug

`START TRANSACTION READ ONLY` puts RepoDB's `transaction` into read-only mode (`engine/catalog.go`, `IsReadOnly()`). When a transaction is read-only, go-mysql-server runs its `validateReadOnlyTransaction` analyzer rule on each statement. For a write (`InsertInto`, `Update`, `DeleteFrom`), the rule visits the target tables and asks whether each one is temporary, because MySQL allows writes to temporary tables in a read-only transaction:

```go
tt, isTempTable := table.(sql.TemporaryTable)
if !isTempTable { valid = false }
return tt.IsTemporary()   // tt is nil when the assertion failed
```

The rule marks the statement invalid correctly, but then calls `IsTemporary()` on the nil result of the failed type assertion. RepoDB's `*table` did not implement `sql.TemporaryTable`, so every write in a read-only transaction panicked during analysis:

- **Embedded:** `Session.Query` doesn't recover panics, so the host process died.
- **Server:** GMS's handler recovers panics (`mysql_server caught panic`). The server survived, but the client received a generic error instead of MySQL's 1792.

## The fix

`*table` now implements `sql.TemporaryTable`:

```go
func (*table) IsTemporary() bool { return false }
var _ sql.TemporaryTable = (*table)(nil)
```

RepoDB has no temporary tables, so `false` is correct. The type assertion now succeeds, and the rule returns `sql.ErrReadOnlyTransaction`. GMS's error mapping turns that into code 1792 over the wire.

### Why not fix it elsewhere

- **Replace the GMS rule.** Not possible: the rule ID is unexported and wired into GMS's built-in batches.
- **Recover panics in `Session.Query`.** Treats the symptom, would hide other bugs, and still wouldn't produce error 1792.
- **Patch GMS upstream.** Worth doing eventually, but RepoDB pins a GMS version and needs the fix now. The interface implementation is correct regardless of what GMS does.

## Edge case checked

GMS's own tables (`dual`, `information_schema`) also don't implement `TemporaryTable`, so in principle a write whose plan includes one could panic the same way. Probes in a READ ONLY transaction (`DELETE … WHERE id IN (SELECT … FROM information_schema.columns)`, `UPDATE … SET n = (SELECT 1 FROM dual)`, and `UPDATE t JOIN information_schema.tables …`) all returned the read-only error. No separate bug was needed.

## Tests

- `engine/readonly_test.go`, embedded: `INSERT`, `UPDATE`, `DELETE` and `INSERT … SELECT` fail with `ErrReadOnlyTransaction`; `SELECT` works inside the transaction; after `COMMIT` no rejected change is visible, and the session can write again. It panicked before the fix.
- `server/readonly_test.go`, over the wire: uses `go-sql-driver/mysql` directly with a pinned `db.Conn`, because transaction state is per connection and `client.Client` pools connections. It asserts `MySQLError.Number == 1792` for each write, then that reads, `COMMIT` and a later write work on the same connection.

## Docs

- `docs/sql.md`: the read-only transactions entry now describes the behavior: reads allowed, writes fail with 1792, transaction stays open.
- `docs/testing.md`: the "read-only transactions untested" gap is removed; the engine and server suite sections list the new coverage.
