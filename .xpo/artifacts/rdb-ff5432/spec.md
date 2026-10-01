# rdb-ff5432: Write inside READ ONLY transaction panics

## What

A write inside `START TRANSACTION READ ONLY` crashes the process with a nil pointer dereference instead of returning error 1792.

## Why (root cause, confirmed by reading go-mysql-server)

`sql/analyzer/validation_rules.go`, `validateReadOnlyTransaction`:

```go
isTempTable := func(table sql.Table) bool {
    tt, isTempTable := table.(sql.TemporaryTable)
    if !isTempTable { valid = false }
    return tt.IsTemporary()   // line 921: tt is nil when the assertion failed
}
```

RepoDB's `*table` (`engine/catalog.go`) does not implement `sql.TemporaryTable`, so the assertion fails and `tt.IsTemporary()` is called on a nil interface. The rule ID is unexported and hard-wired into GMS's batches, so RepoDB can't replace the rule.

Embedded, the panic kills the host process. Over the wire, GMS's handler recovers it (`mysql_server caught panic`): the server survives, but the client gets a generic error instead of 1792.

## How

- Implement `IsTemporary() bool { return false }` on `*table` and add `var _ sql.TemporaryTable = (*table)(nil)`. The rule then sets `valid = false` and returns `sql.ErrReadOnlyTransaction`, which GMS maps to code 1792.
- RepoDB has no temporary tables, so `false` is correct.

## Tests

- `engine/readonly_test.go`: in a READ ONLY transaction, `INSERT`, `UPDATE`, `DELETE` and `INSERT … SELECT` return `sql.ErrReadOnlyTransaction`. `SELECT` works. After `COMMIT`, the rejected changes are absent and the session can write again.
- `server/readonly_test.go`: on one pinned connection, `INSERT`/`UPDATE`/`DELETE` return MySQL error 1792. The connection keeps serving reads, `COMMIT` and a later write.

## Edge case (checked, no bug)

Other non-temporary tables (GMS `dual`, `information_schema`) inside a write's plan could in principle hit the same GMS nil dereference. Probed in a READ ONLY transaction, with the fix applied:

- `DELETE … WHERE id IN (SELECT … FROM information_schema.columns)`
- `UPDATE t SET n = (SELECT 1 FROM dual)`
- `UPDATE t JOIN information_schema.tables …`

All three return the read-only error without panicking. No separate bug was filed.

## Acceptance

- Write in a READ ONLY transaction returns 1792 without panicking, embedded and over the wire.
- Reads in READ ONLY transactions work.
- Regression tests cover both.
- `docs/sql.md` describes the read-only behavior, and `docs/testing.md` lists the new coverage instead of the gap.
