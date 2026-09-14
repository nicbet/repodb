# Walkthrough: Support DEFAULT values for columns

## What was built

RepoDB now supports the `DEFAULT` clause on columns in `CREATE TABLE` and `ALTER TABLE ADD COLUMN`. Default values are persisted in the on-disk schema, applied automatically on INSERT when a column is omitted, and used to backfill existing rows when adding a NOT NULL column with a default.

## How the pieces fit together

The implementation touches only `engine/catalog.go` — five small, focused edits — because go-mysql-server already handles the heavy lifting for both INSERT defaults and ALTER TABLE backfill. RepoDB's job is to persist the default expression and stop rejecting it.

### Persistence (`columnDisk` / `encodeSchema` / `decodeSchema`)

Default expressions are stored as their SQL string representation alongside two boolean flags:

```json
{"name":"score","type":3,"nullable":false,"default":"0","default_literal":true}
{"name":"val","type":3,"nullable":false,"default":"((2 + 2) / 2)","default_paren":true}
```

- `default`: the SQL expression text from `ColumnDefaultValue.String()`
- `default_literal`: true for literal defaults (`DEFAULT 42`), false for expression defaults
- `default_paren`: true for parenthesized expression defaults (`DEFAULT (expr)`)

All three fields use `json:",omitempty"`, so existing schemas without defaults decode with zero values — no migration needed.

On decode, defaults are reconstructed via `sql.NewUnresolvedColumnDefaultValue(exprString)` with the stored `Literal` and `Parenthesized` flags. go-mysql-server's analyzer resolves the unresolved default into a proper expression tree during query planning — the integrator never needs to resolve expressions itself.

### INSERT flow (no RepoDB changes)

go-mysql-server's `wrapRowSource` analyzer rule (inserts.go:131) builds a projection that substitutes `col.Default` for any column omitted from the INSERT column list. By the time RepoDB's `editor.Insert` is called, the row is complete with defaults filled in. This works automatically as long as `col.Default` is populated on the schema — which it now is after decoding.

### ALTER TABLE ADD COLUMN backfill (no RepoDB changes)

go-mysql-server's `addColumnIter.Next()` (ddl_iters.go:1248) calls `alterable.AddColumn()` first, then — if the column is non-nullable or has a default — calls `UpdateRowsWithDefaults()`, which iterates the table via `UpdatableTable.Updater()` and applies the resolved default to each existing row. RepoDB already implements `UpdatableTable` (catalog.go:1356), so backfill works automatically.

The only change in RepoDB's `AddColumn` is relaxing the NOT NULL guard: it now rejects only when `!column.Nullable && column.Default == nil && len(rows) > 0`, allowing the operation when a default is present.

### Validation (`validateSchema`)

Removed `col.Default != nil` from the rejection condition. `AutoIncrement` and `Generated` remain blocked. This single change unblocks CREATE TABLE, AddColumn, ModifyColumn, and both merge validation paths (merge.go:141, merge.go:416) simultaneously.

## Key decisions

**String-based serialization over AST serialization**: We store the default as its SQL text rather than serializing the expression tree. This is simpler and version-resilient — we don't couple to go-mysql-server's internal expression types. The tradeoff is reliance on `String()` round-tripping correctly. We verified empirically that `Arithmetic.String()` wraps each binary operation in parentheses (e.g., `((2 + 2) / 2)`), so nested arithmetic preserves grouping. A dedicated reopen test (`TestExpressionDefaultRoundTripThroughReopen`) guards against future regressions in this property.

**No backfill logic in RepoDB**: Rather than evaluating defaults ourselves in `AddColumn`, we let go-mysql-server's `addColumnIter` handle it via the existing `UpdatableTable` interface. This keeps the integrator thin and avoids duplicating expression evaluation logic.

## Test coverage

Nine new tests in `engine/engine_test.go`:

| Test | What it covers |
|------|---------------|
| `TestCreateTableWithDefaultLiteral` | String and int literal defaults on INSERT with omitted columns |
| `TestCreateTableWithDefaultExpression` | Parenthesized expression default `(1 + 1)` |
| `TestExpressionDefaultRoundTripThroughReopen` | Nested arithmetic `((2+2)/2)` survives persist/reopen |
| `TestInsertExplicitDefaultKeyword` | `INSERT INTO t VALUES (1, DEFAULT)` |
| `TestAlterTableAddNotNullWithDefault` | Backfills existing rows with the default value |
| `TestAlterTableAddNotNullNoDefaultStillRejected` | Regression guard for NOT NULL without DEFAULT |
| `TestDefaultPersistsThroughReopen` | Working-state commit persistence round-trip |
| `TestDefaultPersistsNativeGit` | Native git commit persistence round-trip |