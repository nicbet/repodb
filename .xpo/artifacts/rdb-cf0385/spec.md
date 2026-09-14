# Spec: ALTER TABLE support

## What

Implement `sql.AlterableTable` on the `table` struct in `engine/catalog.go` so that go-mysql-server's existing ALTER TABLE parsing flows through to RepoDB. Today, ALTER TABLE is parsed successfully but fails at execution with "table cannot be altered" because `table` does not implement the interface.

## Why

Consumers like nate must drop and recreate tables to change a single column, losing all existing data. ALTER TABLE enables non-destructive schema migrations.

## Scope

**In scope:**
- `ALTER TABLE ... ADD COLUMN` (with FIRST / AFTER positioning)
- `ALTER TABLE ... DROP COLUMN`
- `ALTER TABLE ... MODIFY COLUMN` (type change, nullability, rename via this path)
- `ALTER TABLE ... RENAME COLUMN`
- Row re-encoding for all existing rows when column layout changes
- Schema persistence through both journal and native-git commit paths
- Tests for each operation, including round-trip through engine close/reopen

**Out of scope:**
- `RENAME TABLE` (separate `sql.TableRenamer` interface on `database` — file as follow-up if needed)
- DEFAULT values, AUTO_INCREMENT, generated columns (rejected by `validateSchema`)
- Index-related ALTER operations (separate epic rdb-a65375)
- PRIMARY KEY changes (separate `sql.PrimaryKeyAlterableTable` — high complexity, defer)

## How

### 1. Implement `sql.AlterableTable` on `table`

Add three methods plus a compile-time assertion:

```go
var _ sql.AlterableTable = (*table)(nil)
```

**`AddColumn(ctx, column, order)`:**
- Validate via `validateSchema` (reject AutoIncrement, Generated, unsupported types)
- Reject adding a NOT NULL column without a default (existing rows would have no value — MySQL rejects this too)
- Build new schema: append column, or insert at position per `order` (First → index 0, AfterColumn → after named column)
- Rewrite all in-memory rows: insert NULL at the new column's position
- Update `t.state.schema`, mark dirty

**`DropColumn(ctx, columnName)`:**
- Reject dropping a PRIMARY KEY column (would break row identity)
- Find column index, remove from schema
- Rewrite all in-memory rows: remove cell at that index
- Update PK ordinals if they shift
- Mark dirty

**`ModifyColumn(ctx, columnName, column, order)`:**
- Validate new type is supported
- If type changed: convert each existing row's value at that column position (use `column.Type.Convert()`)
- If position changed (order != nil): reorder schema and rewrite rows
- If name changed: update schema column name
- Reject changing a PK column's type (would require re-keying every row)
- Mark dirty

### 2. Row rewriting helper

All three methods need to rewrite existing rows. Factor out a helper:

```go
func (t *table) rewriteRows(ctx context.Context, tx *transaction, transform func(sql.Row) (sql.Row, error)) error
```

This calls `t.state.ensureRows(ctx)` to load all rows into memory, then iterates `t.state.rows`, applies the transform, re-encodes the primary key, and stores the result back. Each modified row becomes an edit in `t.state.edits` so the normal commit path picks it up.

### 3. Fix journal schema persistence for existing tables

In `commitTypedEdits` (catalog.go:294-300), the condition currently only sends schema bytes for new tables:

```go
if !state.manifest.DataRoot.Valid() && !state.manifest.SchemaRoot.Valid() {
```

Change to also send schema bytes when the schema has been modified. Track this with a `schemaDirty` flag on `tableState`, set by the ALTER methods. The journal's `applyTypedEditsToSnapshot` (working.go:453) already handles updating the schema hash when `te.Schema` is provided.

### 4. Native-git path

The native-git commit path (`commitNativeGit`, catalog.go:327+) already re-encodes the schema for dirty tables that have no existing DataRoot (line 387). For ALTER TABLE, the table *does* have an existing DataRoot. Need to also re-encode schema when `schemaDirty` is true — the schema blob gets a new hash, the old one becomes unreachable, and the manifest entry is updated.

## Constraints

- **PK columns cannot be dropped or have their type changed.** PK values are binary-encoded as row keys in the prolly tree. Changing PK structure requires re-keying every entry, which is a much larger change (deferred).
- **ADD COLUMN with NOT NULL and no default must be rejected.** Existing rows have no value for the new column. MySQL allows this with a zero-value default in strict mode, but we should reject it to avoid silent data corruption.
- **Row rewriting loads all rows into memory.** This is consistent with how the engine already works (full table scans decode all rows). Not a new limitation, but worth noting for future large-table support.
- **Column ordering (FIRST/AFTER) support.** go-mysql-server passes `*sql.ColumnOrder` — we must respect it since it affects row cell positions. If nil, append to the end (ADD) or keep position (MODIFY).

## Acceptance criteria

- [ ] `ALTER TABLE t ADD COLUMN c TYPE` works, with FIRST/AFTER positioning
- [ ] `ALTER TABLE t DROP COLUMN c` works, rejects PK columns
- [ ] `ALTER TABLE t MODIFY COLUMN c NEW_TYPE` works, converts existing values
- [ ] `ALTER TABLE t RENAME COLUMN old TO new` works
- [ ] ADD NOT NULL column without existing data succeeds; ADD NOT NULL with existing rows is rejected
- [ ] Schema changes persist through journal commit path (close + reopen engine)
- [ ] Schema changes persist through native-git commit path
- [ ] Existing row data is preserved correctly after each operation
- [ ] All existing tests continue to pass
- [ ] PK column drop/type-change is rejected with a clear error
