# Walkthrough: CHECK constraints

## What was built

RepoDB now supports CHECK constraints in `CREATE TABLE` and `ALTER TABLE`. Check definitions are persisted in the on-disk schema, enforced on INSERT and UPDATE by go-mysql-server, and visible in `SHOW CREATE TABLE`.

## How the pieces fit together

### Storage model

Check definitions are stored alongside column metadata in the existing `schemaDisk` JSON blob:

```json
{
  "columns": [...],
  "primary_key": [0],
  "checks": [
    {"name": "t_chk_1", "expression": "(age >= 0)", "enforced": true}
  ]
}
```

The `checks` field uses `json:",omitempty"` — existing schemas without checks decode cleanly. Each check is a simple triple: name, expression string, enforced flag. go-mysql-server owns the expression parsing and evaluation; RepoDB only stores the opaque string.

In-memory, `tableState` carries a `checks []sql.CheckDefinition` slice. The metadata cache (`cachedTableMeta`) also carries checks so they survive across auto-commit transaction boundaries.

### Interfaces implemented

- **`sql.CheckTable`** — `GetChecks()` returns stored check definitions
- **`sql.CheckAlterableTable`** — `CreateCheck()` / `DropCheck()` modify the checks slice and mark the table dirty

### How go-mysql-server uses these

1. **CREATE TABLE**: engine creates the table, then calls `CreateCheck()` for each CHECK in the DDL
2. **ALTER TABLE ADD CHECK**: engine validates existing rows against the expression, then calls `CreateCheck()`
3. **INSERT/UPDATE enforcement**: plan builder calls `GetChecks()`, parses each expression, attaches resolved checks to the plan node — the row iterator evaluates them and returns `ErrCheckConstraintViolated` on failure
4. **SHOW CREATE TABLE**: reads `GetChecks()` to emit CHECK clauses
5. **DROP CHECK/CONSTRAINT**: analyzer validates the name exists via `GetChecks()`, then calls `DropCheck()`

### `encodeSchema` / `decodeSchema` signature change

Both functions gained a checks parameter:

```go
func encodeSchema(schema sql.PrimaryKeySchema, checks []sql.CheckDefinition) ([]byte, error)
func decodeSchema(data []byte) (sql.PrimaryKeySchema, []sql.CheckDefinition, error)
```

All callers updated: three commit paths in `catalog.go`, `loadTableMetadata`, and two sites in `merge.go`.

## Key decisions and fixes

### Unnamed check name generation

go-mysql-server passes an empty `CheckDefinition.Name` for unnamed constraints and expects the integrator to generate one. `CreateCheck` generates names following MySQL convention: `<table>_chk_<n>`, using case-insensitive matching against existing names to find the next available number. This ensures `T_CHK_1` and `t_chk_1` are recognized as the same sequence.

### Duplicate name rejection

`CreateCheck` rejects explicitly named constraints that collide case-insensitively with an existing check, matching `DropCheck`'s `EqualFold` lookup semantics.

### Slice isolation from the metadata cache

The check slice is shared between `cachedTableMeta` and `tableState`. Three operations could mutate the backing array and corrupt the cache:

- `DropCheck` — originally used `append([:i], [i+1:]...)` which mutates in place
- `CreateCheck` — `append` can share the backing array if capacity allows
- Cache read — assigning the same slice to a new transaction

Fixes:
- `DropCheck` allocates a fresh slice instead of splicing in place
- `CreateCheck` calls `copyChecks()` before appending
- Cache hit path copies checks via `copyChecks()` when constructing transaction state
- Cache miss path copies checks via `copyChecks()` when storing to cache

### NOT NULL on non-PK columns

Already enforced by go-mysql-server — added test coverage to verify (`TestNotNullOnNonPKColumn`).

## Test coverage

12 new tests in `engine/engine_test.go`:

| Test | What it covers |
|------|---------------|
| `TestCheckConstraintOnCreateTable` | Unnamed CHECK, enforcement, generated name in SHOW CREATE TABLE, drop by generated name |
| `TestCheckConstraintNamed` | Named CONSTRAINT on CREATE TABLE |
| `TestCheckConstraintOnUpdate` | CHECK enforced on UPDATE |
| `TestAlterTableAddCheck` | ALTER TABLE ADD CONSTRAINT ... CHECK |
| `TestAlterTableAddCheckRejectsExistingViolations` | Rejects when existing rows violate |
| `TestAlterTableDropCheck` | DROP CHECK removes enforcement |
| `TestDropCheckDoesNotCorruptCache` | Drop first of two checks, second still enforced |
| `TestCheckConstraintPersistsThroughReopen` | Working-state persistence round-trip |
| `TestCheckConstraintPersistsNativeGit` | Native git persistence round-trip |
| `TestShowCreateTableIncludesCheck` | SHOW CREATE TABLE includes CHECK clause |
| `TestNotNullOnNonPKColumn` | NOT NULL enforcement on non-PK columns |