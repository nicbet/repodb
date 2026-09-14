# Walkthrough: ALTER TABLE support

## What was built

RepoDB now supports `ALTER TABLE` for non-destructive schema migrations via the `sql.AlterableTable` interface. go-mysql-server already parsed ALTER TABLE statements — this change implements the three methods it calls back into:

- `ALTER TABLE ... ADD COLUMN` (with `FIRST` / `AFTER` positioning)
- `ALTER TABLE ... DROP COLUMN`
- `ALTER TABLE ... MODIFY COLUMN` (type conversion, nullability change, rename, reorder)
- `ALTER TABLE ... RENAME COLUMN` (routed through ModifyColumn by go-mysql-server)

## How the pieces fit

```
SQL: ALTER TABLE t ADD COLUMN age BIGINT
  → go-mysql-server parser (already existed)
  → plan.AddColumn node (already existed)
  → executor casts table to sql.AlterableTable (NEW: now succeeds)
  → table.AddColumn()
    1. validateSchema — reject unsupported types, auto-increment, defaults, generated
    2. reject PrimaryKey constraint on added column
    3. ensureRows — load all rows into memory
    4. reject NOT NULL on populated table (no DEFAULT support yet)
    5. build new schema with column at the right position
    6. rewrite all rows — insert NULL at new column position
    7. mark state dirty + schemaDirty
  → implicit DDL commit
    → commitTypedEdits or commitNativeGit
      → schemaDirty flag triggers schema re-encoding (the fix)
```

The same flow applies to DropColumn and ModifyColumn, each with their own validation and row transformation.

## Key design decisions

### Row rewriting

Rows are stored as positional cell arrays (`[]cellDisk` in JSON). Any column layout change requires rewriting every existing row. The ALTER methods call `ensureRows()` to load all rows, apply a transformation (insert/remove/reorder cells), and store the results as edits. The normal commit path then persists them.

This is consistent with how the engine already works — table scans decode all rows into memory. Not a new limitation, but it means ALTER TABLE on a large table is O(n) in memory and I/O.

### Schema persistence fix

Both commit paths had a gap: they only encoded schema bytes for brand-new tables (no existing SchemaRoot). A new `schemaDirty` flag on `tableState` triggers schema re-encoding for existing tables too. The journal path's `applyTypedEditsToSnapshot` already handled incoming schema bytes correctly — only the producer side needed the fix.

### Validation guards

Six categories of invalid ALTER are rejected before any mutation:

1. **Unsupported types** — `validateSchema` rejects DATE, TIMESTAMP, etc. (same check as CREATE TABLE)
2. **PK column via ADD** — inline PRIMARY KEY constraint on ADD COLUMN is rejected
3. **PK column drop** — cannot drop a primary key column
4. **PK type/length change** — uses `Type.Equals()` (not `Type.Type()`) to catch parameterized differences like VARCHAR(100) → VARCHAR(10)
5. **NOT NULL on populated table** — no DEFAULT mechanism yet, so existing rows would have no value
6. **Nullable → NOT NULL with existing NULLs** — scans rows to verify no NULLs exist before tightening

### Type change detection

Uses `Type.Equals()` rather than `Type.Type()` for comparing old and new column types. `Type.Type()` only compares the base Vitess enum (e.g. both VARCHAR(100) and VARCHAR(10) return `VARCHAR`), missing parameterized differences. `Type.Equals()` compares the full type including length, so narrowing a VARCHAR or changing any type parameter triggers conversion and validation.

The `inRange` flag from `column.Type.Convert()` is checked — narrowing conversions that would truncate values are rejected rather than silently clamping.

## What's deferred

- **PK column changes** (drop, type change, re-keying) → rdb-1ced7f
- **DEFAULT values** (enables ADD NOT NULL on populated tables) → rdb-68106e
- **RENAME TABLE** → separate `sql.TableRenamer` interface on `database`, not filed yet

## Test coverage

23 tests in `engine/engine_test.go`:

| Test | Validates |
|------|-----------|
| AddColumn | Basic add, NULL backfill, insert with new column |
| AddColumnFirst | FIRST positioning |
| AddColumnAfter | AFTER positioning |
| AddNotNullRejectsWithRows | NOT NULL on populated table rejected |
| AddPrimaryKeyColumnRejected | Inline PK constraint rejected |
| AddUnsupportedTypeRejected | DATE column rejected by validateSchema |
| ModifyToUnsupportedTypeRejected | MODIFY to DATE rejected |
| ModifyNullableToNotNullWithNullsRejected | Tightening with existing NULLs rejected |
| ModifyNullableToNotNullNoNulls | Tightening without NULLs accepted, enforced on insert |
| AddNotNullEmptyTable | NOT NULL on empty table succeeds |
| DropColumn | Basic drop, data preserved |
| DropPKColumnRejected | PK column drop rejected |
| ModifyVarcharNarrowingValidates | VARCHAR(100)→VARCHAR(10) with oversized data rejected |
| ModifyPKLengthRejected | PK VARCHAR length change rejected |
| ModifyColumnType | BIGINT→TEXT conversion |
| ModifyPKTypeRejected | PK type change rejected |
| ModifyColumnFirst | MODIFY ... FIRST reorders schema and rows |
| ModifyColumnAfter | MODIFY b ... AFTER c produces correct order |
| ModifyNarrowingConversionRejected | BIGINT(99999)→TINYINT rejected |
| ModifyReorderPersists | Reordered schema survives engine close/reopen |
| RenameColumn | RENAME COLUMN preserves data |
| PersistsThroughReopen | Journal persistence round-trip |
| PersistsNativeGit | Native-git persistence round-trip (ADD + DROP) |
