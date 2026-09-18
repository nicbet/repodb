# Walkthrough: ALTER TABLE on primary key columns

## What was built

RepoDB now supports `ALTER TABLE ... DROP COLUMN` on PK columns (composite PKs) and `ALTER TABLE ... MODIFY COLUMN` with type changes on PK columns. Both operations re-key every row in the prolly tree and rebuild all secondary indexes.

Previously, these operations were rejected with hard-coded guards in `DropColumn` and `ModifyColumn`.

## How the pieces fit together

### The `rekeyRows` function

The core addition is a standalone pure function in `engine/catalog.go`:

```go
func rekeyRows(schema sql.PrimaryKeySchema, oldRows map[string]sql.Row, oldEdits map[string]rowEdit) (map[string]sql.Row, map[string]rowEdit, error)
```

It takes old rows (keyed by old encoded PKs), old edits (the transaction's pending edit map), and the new schema. It returns re-keyed rows and a combined edit map, or a duplicate-key error.

The combined edit map contains three layers:
1. **Preserved deletes** from `oldEdits` — rows deleted earlier in the same transaction
2. **Old-key deletes** for live rows whose keys changed
3. **New-key inserts** for all live rows

This layering is essential. Without preserved deletes (layer 1), a `BEGIN; DELETE ...; ALTER TABLE ...; COMMIT` sequence would resurrect deleted rows. Without old-key deletes (layer 2), the commit path (`prolly.Apply` or journal) would leave stale entries in the persisted tree alongside the new entries.

Being a pure function (not a mutating method) enables the validate-before-mutate pattern described below.

### DropColumn changes

The PK guard was replaced with a conditional:
- Single PK column → reject ("cannot drop the only primary key column")
- Composite PK → proceed with re-keying

The method builds the candidate schema and candidate rows (cells removed) as local variables, then calls `rekeyRows` to validate. If duplicate keys are detected, it returns the error immediately — no `tableState` fields have been touched. Only after validation passes does it install the new schema, adjusted index ordinals, re-keyed rows, and re-keyed edits atomically.

The PK ordinal calculation now has two paths: for PK columns, the dropped ordinal is removed from `PkOrdinals` (shrinking the slice by one); for non-PK columns, ordinals are shifted down as before.

### ModifyColumn changes

The PK type-change guard was removed. The existing type conversion code (value conversion, range checking, unique index constraint validation) runs unchanged — it already works correctly for PK columns.

A pre-mutation validation gate was added: after building `convertedRows` and checking unique indexes (all read-only operations), the method builds a candidate schema and candidate rows with conversions applied, and calls `rekeyRows` to check for duplicates. If validation fails, no state has been mutated.

After the existing mutations (apply conversions, update schema, optional reorder), a final `rekeyRows` call computes the actual re-keyed state using the post-mutation schema and rows. This call cannot fail because reorder doesn't change PK values — it moves cells and updates `PkOrdinals` in lockstep, so `encodeKey` produces identical keys before and after reorder.

### Index rebuild

Both operations set `manifest.Indexes = nil`, which forces `commitIndexTrees` → `buildIndexTreeForDef` to rebuild every secondary index from scratch at commit time. This is necessary even for indexes that don't directly reference the changed column, because:
- Every index **value** is the encoded PK bytes
- Every non-unique index **key** includes the PK as a suffix

## Key decisions and their rationale

**Pure function over mutating method.** The original implementation used a `rekeyAllRows` method on `tableState` that mutated in place. Review caught that this left partially mutated state on failure and lost pending delete edits. The standalone `rekeyRows` function fixes both: callers validate with it before mutating, and it explicitly preserves delete edits from the input.

**Pre-validation in ModifyColumn.** Rather than restructuring the entire method to defer all mutations, a lightweight validation pass runs before any state changes. It builds throwaway candidate rows/schema, calls `rekeyRows`, and discards the result. The actual re-keying runs at the end after all mutations. This keeps the existing code structure intact while guaranteeing atomicity.

**Combined delete+insert edits.** The alternative — invalidating `DataRoot` to force a full `prolly.Build` from `state.rows` — works for the native-git commit path but has unclear semantics for the journal path when a table previously had persisted data. The edit-based approach works identically for both paths.

## Anything non-obvious

The `rekeyRows` function preserves ALL delete edits from the old edit map, even if their keys happen to collide with new insert keys. Map assignment order handles this correctly: deletes are written first, then inserts overwrite any collision. This means a re-keyed row that happens to land on a previously-deleted key gets the insert (correct behavior — the new row replaces the old deleted one).

The atomicity tests use explicit transactions (`s.Begin()` → `tx.Exec()`) with a successful INSERT before the failing ALTER TABLE. This ensures the transaction is dirty with prior work, which is the scenario where partial mutation would be visible. A clean autocommit transaction would discard partial state on failure regardless of the fix.
