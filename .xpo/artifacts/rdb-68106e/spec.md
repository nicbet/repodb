# Spec: Support DEFAULT values for columns

## What

Add support for the `DEFAULT` clause on columns in `CREATE TABLE` and `ALTER TABLE ADD COLUMN`. Persist default expressions in the on-disk schema, apply them on INSERT for omitted columns, and use them to backfill existing rows when adding a NOT NULL column with a default.

## Why

Without DEFAULT, `ALTER TABLE ... ADD COLUMN ... NOT NULL` is rejected on tables with existing rows (there's no value to fill in). This blocks non-destructive schema migrations on populated tables — a key gap identified in the ALTER TABLE work (rdb-cf0385). DEFAULT is also a basic SQL feature consumers expect from CREATE TABLE.

## Scope

**In scope:**
- Literal DEFAULT values (`DEFAULT 42`, `DEFAULT 'hello'`, `DEFAULT NULL`)
- Expression DEFAULT values (`DEFAULT (NOW())`, `DEFAULT (col1 + 1)`)
- Persistence in the on-disk schema format (`columnDisk`)
- INSERT behavior: go-mysql-server already fills in defaults from `col.Default` — RepoDB just needs the value on the schema
- ALTER TABLE ADD COLUMN with NOT NULL + DEFAULT: go-mysql-server handles backfill via `UpdateRowsWithDefaults` using RepoDB's existing `UpdatableTable` interface — RepoDB just needs to stop rejecting the operation
- Merge paths: `validateSchema` in merge.go automatically allows DEFAULT once the check is lifted

**Out of scope:**
- `AutoIncrement` and `Generated` columns (remain blocked)
- `ALTER TABLE MODIFY COLUMN` to add/change/remove a default (would work if the schema round-trips correctly, but not explicitly tested or guaranteed in this issue)
- Expression defaults that reference other columns (go-mysql-server resolves these; RepoDB stores the expression string and re-parses it — if the parser round-trips it, it works)

## Design

### Serialization strategy

Store the default as its SQL string representation using `ColumnDefaultValue.String()`. On decode, reconstruct via `sql.NewUnresolvedColumnDefaultValue(expr)` — go-mysql-server's analyzer resolves unresolved defaults during query planning, so the integrator doesn't need to resolve expressions itself.

Also persist two boolean flags (`literal` and `parenthesized`) so the reconstructed `ColumnDefaultValue` has the correct form. The `Literal` flag distinguishes `DEFAULT 42` from `DEFAULT (42)`, which affects how go-mysql-server formats and resolves the expression.

### Schema changes

**`columnDisk` struct** — add three fields:

```go
type columnDisk struct {
    Name          string `json:"name"`
    Type          int32  `json:"type"`
    Length        int64  `json:"length,omitempty"`
    Nullable      bool   `json:"nullable"`
    Default       string `json:"default,omitempty"`
    DefaultLit    bool   `json:"default_literal,omitempty"`
    DefaultParen  bool   `json:"default_paren,omitempty"`
}
```

The `omitempty` tags ensure backward compatibility — existing schemas without defaults decode cleanly with zero-value fields.

### Code changes

1. **`validateSchema`** (`catalog.go:1166`): Remove `col.Default != nil` from the rejection condition. Keep `AutoIncrement` and `Generated` blocked.

2. **`encodeSchema`** (`catalog.go:1097`): When `col.Default != nil`, serialize `Default: col.Default.String()`, `DefaultLit: col.Default.IsLiteral()`, `DefaultParen: col.Default.IsParenthesized()`.

3. **`decodeSchema`** (`catalog.go:1109`): When `cd.Default != ""`, reconstruct:
   ```go
   defVal := sql.NewUnresolvedColumnDefaultValue(cd.Default)
   defVal.Literal = cd.DefaultLit
   defVal.Parenthesized = cd.DefaultParen
   col.Default = defVal
   ```

4. **`AddColumn`** (`catalog.go:685-686`): Replace the NOT NULL rejection:
   - If `!column.Nullable && column.Default != nil`: allow it — go-mysql-server's `addColumnIter.UpdateRowsWithDefaults()` handles backfilling existing rows via RepoDB's `UpdatableTable`/`Updater()` interface after `AddColumn` returns.
   - If `!column.Nullable && column.Default == nil && len(t.state.rows) > 0`: keep the existing rejection.
   - The `nil` placeholder at line 722 is fine — go-mysql-server overwrites it immediately via the updater.

### How go-mysql-server handles backfill (no RepoDB changes needed)

go-mysql-server's `addColumnIter.Next()` (ddl_iters.go:1248):
1. Calls `alterable.AddColumn(ctx, column, order)` — our method inserts `nil` for existing rows
2. If the column is non-nullable or has a default, calls `UpdateRowsWithDefaults()`
3. `UpdateRowsWithDefaults` uses `UpdatableTable.Updater()` to iterate and update each row with the evaluated default
4. RepoDB already implements `UpdatableTable` (`catalog.go:1356`)

### How go-mysql-server handles INSERT defaults (no RepoDB changes needed)

go-mysql-server's analyzer `wrapRowSource` (inserts.go:131) builds a projection that substitutes `col.Default` for omitted columns. RepoDB's `editor.Insert` receives a complete row with defaults already filled in.

### Backward compatibility

- Existing on-disk schemas have no `default*` JSON fields. `omitempty` + zero values means `decodeSchema` reads them as no-default, which is correct.
- No migration needed — the format is additive.

## Flow

1. User issues `CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(100) DEFAULT 'unknown')`
2. go-mysql-server parses and populates `sql.Column.Default`
3. `validateSchema` passes (no longer rejects Default)
4. `encodeSchema` persists `{"default":"'unknown'","default_literal":true}` in the column JSON
5. On next open, `decodeSchema` reconstructs the `ColumnDefaultValue` as unresolved
6. `INSERT INTO t (id) VALUES (1)` — go-mysql-server's analyzer resolves the default and fills `'unknown'` for the `name` column before calling `editor.Insert`
7. `ALTER TABLE t ADD COLUMN score INT NOT NULL DEFAULT 0` — `AddColumn` inserts `nil` placeholders, then go-mysql-server's `UpdateRowsWithDefaults` backfills `0` via the updater

## Acceptance criteria

- [ ] `CREATE TABLE` with DEFAULT literal values works and persists through close/reopen
- [ ] `CREATE TABLE` with DEFAULT expression values (parenthesized) works and persists
- [ ] `INSERT` with omitted columns uses the default value
- [ ] `INSERT` with explicit `DEFAULT` keyword uses the default value
- [ ] `ALTER TABLE ADD COLUMN ... NOT NULL DEFAULT <val>` backfills existing rows
- [ ] `ALTER TABLE ADD COLUMN ... NOT NULL` (no default) on populated table still rejected
- [ ] Existing schemas without defaults continue to load correctly (backward compat)
- [ ] Schema with defaults persists correctly through both working-state and native-git commit paths
- [ ] All existing tests pass