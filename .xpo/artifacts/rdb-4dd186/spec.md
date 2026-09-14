# Spec: CHECK constraints

## What

Implement `sql.CheckTable` and `sql.CheckAlterableTable` on RepoDB's `table` struct so that CHECK constraints work in CREATE TABLE and ALTER TABLE. Persist check definitions in the on-disk table metadata.

## Why

CHECK constraints are a basic SQL feature for data integrity. go-mysql-server already parses CHECK clauses and enforces them on INSERT/UPDATE — RepoDB just needs to store and return the definitions. Without this, any CREATE TABLE with a CHECK clause fails silently (the constraint is ignored) or errors.

## Scope

**In scope:**
- `CREATE TABLE ... CHECK (expr)` — named and unnamed
- `ALTER TABLE ... ADD CHECK (expr)` / `ALTER TABLE ... ADD CONSTRAINT name CHECK (expr)`
- `ALTER TABLE ... DROP CHECK name` / `ALTER TABLE ... DROP CONSTRAINT name`
- Persist check definitions through close/reopen (both working-state and native-git paths)
- Merge path compatibility (check definitions survive sync/merge)
- NOT NULL on non-PK columns — verify and add test coverage (already enforced by go-mysql-server)
- `SHOW CREATE TABLE` includes CHECK clauses (handled by go-mysql-server once `GetChecks` works)

**Out of scope:**
- UNIQUE constraints (separate issue rdb-a1f9d1, depends on secondary indexes)
- DEFAULT values (shipped in rdb-68106e)
- CHECK constraints referencing other tables or subqueries (not supported by go-mysql-server)

## Design

### Interfaces to implement

**`sql.CheckTable`** — read path:
```go
func (t *table) GetChecks(ctx *sql.Context) ([]sql.CheckDefinition, error)
```

**`sql.CheckAlterableTable`** — write path:
```go
func (t *table) CreateCheck(ctx *sql.Context, check *sql.CheckDefinition) error
func (t *table) DropCheck(ctx *sql.Context, chName string) error
```

### How go-mysql-server uses these

1. **CREATE TABLE**: after creating the table, the engine casts to `CheckAlterableTable` and calls `CreateCheck()` for each CHECK in the DDL (rowexec/ddl.go:1067-1071)
2. **ALTER TABLE ADD CHECK**: the engine validates existing rows against the expression, then calls `CreateCheck()` (rowexec/ddl_iters.go:1807-1851)
3. **INSERT/UPDATE enforcement**: the plan builder calls `GetChecks()`, parses each expression string, attaches resolved checks to the plan node, and the row iterator evaluates them — RepoDB never evaluates check expressions itself
4. **SHOW CREATE TABLE**: reads `GetChecks()` to include CHECK clauses in output
5. **DROP CHECK/CONSTRAINT**: calls `DropCheck()` by name

### Storage

**`tableState`** — add a field:
```go
type tableState struct {
    // ... existing fields ...
    checks []sql.CheckDefinition
}
```

**`schemaDisk`** — add a field:
```go
type checkDisk struct {
    Name       string `json:"name"`
    Expression string `json:"expression"`
    Enforced   bool   `json:"enforced"`
}

type schemaDisk struct {
    Columns []columnDisk `json:"columns"`
    PK      []int        `json:"primary_key"`
    Checks  []checkDisk  `json:"checks,omitempty"`
}
```

`omitempty` on the Checks slice ensures backward compatibility — existing schemas with no checks decode with a nil slice.

**`encodeSchema`** — serialize checks from the schema's associated state.

Since `encodeSchema` currently takes only a `sql.PrimaryKeySchema` (which doesn't carry checks), we need to either:
- (a) Pass checks as a separate argument to `encodeSchema`, or
- (b) Encode checks separately alongside the schema

Option (a) is simpler — extend the signature:
```go
func encodeSchema(schema sql.PrimaryKeySchema, checks []sql.CheckDefinition) ([]byte, error)
```

**`decodeSchema`** — return checks alongside the schema:
```go
func decodeSchema(data []byte) (sql.PrimaryKeySchema, []sql.CheckDefinition, error)
```

Update all callers of encodeSchema/decodeSchema (catalog.go commit paths + merge.go).

### Code changes

1. **`tableState`**: Add `checks []sql.CheckDefinition` field

2. **`table.GetChecks`**: Return `t.state.checks` (after `ensureRows` to load state)

3. **`table.CreateCheck`**: Append to `t.state.checks`, mark dirty

4. **`table.DropCheck`**: Remove by name from `t.state.checks`, mark dirty. Return error if not found.

5. **`checkDisk` + `schemaDisk`**: New struct, new field on schemaDisk

6. **`encodeSchema`**: Accept checks, serialize them

7. **`decodeSchema`**: Return checks alongside schema

8. **Commit paths** (catalog.go working-state ~line 295, native-git ~lines 384/397): Pass checks to encodeSchema

9. **Load path**: When decoding table state, store checks on tableState

10. **`merge.go`**: Update `decodeAndValidateSchema` and merge logic to pass through checks. For merge conflicts on checks: if both sides modify checks, this is a schema conflict — for now, take a simple last-writer-wins or error approach (checks are additive, so union may be appropriate, but that's a refinement).

11. **Interface assertions**: Add `var _ sql.CheckTable = (*table)(nil)` and `var _ sql.CheckAlterableTable = (*table)(nil)`

12. **`copySchema` or table cloning**: Ensure checks are deep-copied when tableState is cloned

### Merge strategy for checks

During three-way merge, check definitions are identified by name. Strategy:
- If a check exists in local but not remote (and not in base): keep it (added locally)
- If a check exists in remote but not local (and not in base): keep it (added remotely)
- If a check was removed on one side (in base but not in local/remote): remove it
- If both sides added a check with the same name but different expressions: conflict

This mirrors how column-level merge works. For the initial implementation, a simpler approach is acceptable: if the check lists differ and both sides changed, report a conflict.

## Flow

1. `CREATE TABLE t (id BIGINT PRIMARY KEY, age BIGINT, CHECK (age >= 0))`
2. go-mysql-server parses, creates the table, then calls `CreateCheck` with `{Name: "t_chk_1", CheckExpression: "(age >= 0)", Enforced: true}`
3. RepoDB stores it in `tableState.checks`
4. On commit, `encodeSchema` persists `{"checks":[{"name":"t_chk_1","expression":"(age >= 0)","enforced":true}]}`
5. On reopen, `decodeSchema` restores the checks
6. `INSERT INTO t VALUES (1, -5)` — go-mysql-server calls `GetChecks`, parses the expression, evaluates it, returns `ErrCheckConstraintViolated`
7. `INSERT INTO t VALUES (1, 25)` — check passes, row is inserted

## Acceptance criteria

- [ ] `CREATE TABLE` with named CHECK constraint works
- [ ] `CREATE TABLE` with unnamed CHECK constraint works (engine auto-names it)
- [ ] `ALTER TABLE ADD CHECK` works on existing table
- [ ] `ALTER TABLE ADD CHECK` validates existing rows and rejects if any violate
- [ ] `ALTER TABLE DROP CHECK` removes a check by name
- [ ] INSERT violating a CHECK returns a clear error
- [ ] UPDATE violating a CHECK returns a clear error
- [ ] INSERT/UPDATE satisfying all CHECKs succeeds
- [ ] CHECK definitions persist through close/reopen (working-state path)
- [ ] CHECK definitions persist through close/reopen (native-git path)
- [ ] `SHOW CREATE TABLE` includes CHECK clauses
- [ ] NOT NULL on non-PK columns is enforced (test coverage)
- [ ] Existing schemas without checks load correctly (backward compat)
- [ ] All existing tests pass