## What

Support ALTER TABLE operations on primary key columns: dropping a PK column from a composite PK, and changing the type of a PK column via MODIFY COLUMN. Both require re-keying every row in the prolly tree and rebuilding all secondary indexes, because PK values are binary-encoded as tree keys via `encodeKey` and embedded in every index entry.

## Why

Today, `DropColumn` and `ModifyColumn` in `engine/catalog.go` reject any operation that would change the PK encoding. The only way to evolve PK structure is to drop and recreate the table, losing all data. This blocks incremental schema migrations for consumers like nate.

Prerequisite rdb-cf0385 (basic ALTER TABLE) is DONE — this builds directly on its `DropColumn` and `ModifyColumn` methods.

## Acceptance Criteria

- [x] `ALTER TABLE ... DROP COLUMN` on a PK column in a composite PK succeeds when remaining PK columns produce unique keys
- [x] `ALTER TABLE ... DROP COLUMN` on the sole PK column is rejected with a clear error
- [x] `ALTER TABLE ... DROP COLUMN` on a composite PK is rejected when remaining columns produce duplicate keys
- [x] `ALTER TABLE ... MODIFY COLUMN` with a type change on a PK column re-keys all rows
- [x] `ALTER TABLE ... MODIFY COLUMN` type change is rejected when conversion produces duplicate keys
- [x] All secondary indexes (UNIQUE and non-unique) are rebuilt with new PK encoding after either operation
- [x] Data persists correctly through journal commit path (reopen engine)
- [x] Data persists correctly through native-git commit path
- [x] Pending delete edits from earlier in the same transaction are preserved through re-keying
- [x] Failed re-keying (duplicate keys) does not corrupt transaction state

## Flow

### Step 1: Standalone `rekeyRows` function (`catalog.go`)

```go
func rekeyRows(schema sql.PrimaryKeySchema, oldRows map[string]sql.Row, oldEdits map[string]rowEdit) (map[string]sql.Row, map[string]rowEdit, error)
```

A pure function (no state mutation) that:
1. Re-encodes all row keys via `encodeKey(schema, row)` for each row in `oldRows`
2. Detects duplicate keys — returns error if two rows map to the same new key
3. Builds a combined edits map:
   - **Preserves delete edits** from `oldEdits` — rows deleted earlier in the transaction must remain deleted
   - Adds delete edits for old live-row keys no longer present in the new key set
   - Adds insert edits for all new keys (overwrites any colliding delete via map semantics)
4. Returns the new rows map and new edits map, or error

Being a pure function (not a method that mutates `tableState`) is critical: callers use it for validation before mutating state, and also for computing the final re-keyed result.

### Step 2: Modify `DropColumn` (`catalog.go`) — validate-then-install

Replace the blanket PK rejection guard with:

1. If the column is a PK column AND it is the **only** PK column → reject
2. If the column is a PK column AND the PK is composite:
   a. Index-references check (existing)
   b. `ensureRows`
   c. Build candidate schema as local variables: remove column from `Schema`, remove its ordinal from `PkOrdinals`, shift remaining ordinals down
   d. Build candidate rows as local variables: remove the cell at `dropIdx` from each row
   e. **Validate before mutating:** call `rekeyRows(candidateSchema, candidateRows, t.state.edits)` — returns re-keyed rows/edits or duplicate error
   f. If error, return immediately — no state has been touched
   g. Only after validation passes, atomically install: set schema, adjust index ordinals, assign re-keyed rows/edits, clear idxEdits, nil out manifest.Indexes
3. Non-PK columns: existing logic unchanged

### Step 3: Modify `ModifyColumn` (`catalog.go`) — pre-validate then apply

Remove the PK type-change rejection guard. The existing type conversion code already handles value conversion, range checks, and unique index constraint validation.

After building `convertedRows` and checking unique indexes (all read-only), add a **pre-mutation validation gate** for PK type changes:

1. Build candidate schema (copy + update column type) and candidate rows (clone + apply conversions) as local variables
2. Call `rekeyRows(candidateSchema, candidateRows, t.state.edits)` to validate
3. If duplicate key error, return immediately — no state mutated

Then proceed with existing mutations (apply conversions to rows, update schema, optional reorder). At the end:

4. Call `rekeyRows(t.state.schema, t.state.rows, t.state.edits)` with the final post-mutation state to get the actual re-keyed rows/edits
5. Install: assign re-keyed rows/edits, clear idxEdits, nil out manifest.Indexes

The pre-validation at step 2 and the final re-keying at step 4 compute the same PK values (reorder changes cell positions but not PK values; `encodeKey` follows `PkOrdinals`), so step 4 cannot fail if step 2 passed.

### Step 4: Tests (`engine/engine_test.go`)

Updated tests:
- `ModifyPKColumnType` (was `ModifyPKTypeRejected`) — verifies BIGINT→TEXT re-keying succeeds
- `ModifyPKColumnLength` (was `ModifyPKLengthRejected`) — verifies VARCHAR(100)→VARCHAR(50) re-keying succeeds

New tests — feature coverage:
1. **DropPKColumnComposite** — composite PK, drop one column, verify data and PK uniqueness
2. **DropPKColumnDuplicateRejected** — collision detection on composite PK drop
3. **DropPKColumnWithIndex** — secondary index rebuilt after PK column drop
4. **DropPKColumnPersistsJournal** / **DropPKColumnPersistsNativeGit** — persistence
5. **ModifyPKColumnTypeDuplicateRejected** — VARCHAR→BIGINT collision detection
6. **ModifyPKColumnTypeWithIndex** — secondary index rebuilt after PK type change
7. **ModifyPKColumnTypePersistsJournal** / **ModifyPKColumnTypePersistsNativeGit** — persistence
8. **ModifyPKColumnTypeComposite** — composite PK, change one column's type

New tests — correctness bug coverage:
9. **DropPKColumnPreservesTransactionDeletes** — explicit transaction: DELETE then DROP PK column, verify deleted row does not reappear
10. **ModifyPKTypePreservesTransactionDeletes** — explicit transaction: DELETE then MODIFY PK type, verify deleted row does not reappear
11. **DropPKColumnDuplicateDoesNotCorruptState** — explicit transaction: INSERT then failing DROP, verify original schema and all rows (including the insert) remain intact
12. **ModifyPKTypeDuplicateDoesNotCorruptState** — explicit transaction: INSERT then failing MODIFY, verify original types and all rows remain intact

## Decisions

1. **Standalone pure function, not a mutating method.** `rekeyRows` takes old rows, old edits, and new schema as inputs and returns results without side effects. This enables validate-before-mutate: callers check for duplicate keys before touching `tableState`, so a failed re-key leaves zero footprint on transaction state.

2. **Preserve prior delete edits.** `rekeyRows` copies delete edits from `oldEdits` into the output. Rows deleted earlier in an explicit transaction (present in `oldEdits` as deletes, absent from `oldRows` after `ensureRows`) must remain deleted after re-keying. Without this, a `BEGIN; DELETE ...; ALTER TABLE DROP PK COLUMN; COMMIT` sequence would resurrect the deleted row.

3. **Combined delete+insert edits, not DataRoot invalidation.** Both commit paths apply edits against the existing tree. Inserts alone would leave old-key entries in the tree. Delete edits for old keys + insert edits for new keys works cleanly with both `prolly.Apply` (native-git) and journal accumulation.

4. **ALL indexes invalidated on PK re-key, not just directly affected ones.** Even indexes that don't reference the changed column must be rebuilt because: (a) every index value is the encoded PK bytes, and (b) non-unique index keys contain the PK as a suffix. Setting `manifest.Indexes = nil` triggers `buildIndexTreeForDef` for each index at commit time.

5. **Single-column PK drop rejected.** A table must have at least one PK column — this is a hard constraint from go-mysql-server's table contract.

6. **Adding columns to an existing PK is out of scope.** The issue covers DROP and MODIFY on existing PK columns only.

## Edge Cases

**HIGH:**
- **Duplicate keys after re-keying:** Two rows whose remaining PK columns (drop) or converted PK values (modify) are identical. Detected by `rekeyRows` before any state mutation. If not caught, the prolly tree silently loses rows.
- **Pending deletes in transaction:** Rows deleted earlier in an explicit transaction must not reappear after re-keying. `rekeyRows` preserves delete edits from the original edit map.
- **Partial state on failure:** A failed `rekeyRows` (duplicate) must leave the transaction state clean. `DropColumn` validates before installing; `ModifyColumn` pre-validates before mutating.

**MEDIUM:**
- **Type narrowing collision:** e.g., two distinct BIGINT values that map to the same INT after conversion. Caught by duplicate detection in `rekeyRows` after value conversion.
- **Composite PK, one column changes:** Remaining columns encode identically; only the changed column contributes different key bytes. `encodeKey` handles this naturally since it concatenates all PK columns.

**LOW:**
- **Empty table:** Re-keying is a no-op — `ensureRows` produces an empty map, `rekeyRows` returns immediately.
- **Index on dropped PK column:** Already rejected by the existing index-reference check, so re-keying never runs with a stale index definition.

## Assumptions

1. `ensureRows` correctly materializes all rows (persisted + pending edits) before re-keying.
2. `buildIndexTreeForDef` (called at commit when `manifest.Indexes` is nil) correctly rebuilds indexes from the re-keyed `state.rows` using the new schema.
3. `prolly.Apply` handles a mixed delete+insert edit list correctly (both entry types can appear in the same sorted edit slice).
4. go-mysql-server does not call other table methods between `DropColumn`/`ModifyColumn` and commit within the same ALTER TABLE statement execution.
5. Reorder does not change PK values, only their positions in the row array — `encodeKey` follows `PkOrdinals`, so pre-reorder and post-reorder re-keying produce identical keys for the same PK values.
