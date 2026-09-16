## What

Add UNIQUE constraint support backed by dedicated prolly trees — one per constraint. Each index tree is keyed by the constrained column values and stores the corresponding PK, enabling O(log n) uniqueness enforcement on INSERT and UPDATE. The index tree format is designed to carry forward into the secondary indexes epic (`rdb-a65375`) without changes.

## Why

Without UNIQUE constraints, RepoDB can't enforce column-level uniqueness beyond the primary key. This is a basic requirement for real-world schemas (email addresses, usernames, external IDs). Building a proper index tree creates the foundation for general secondary indexes.

## Acceptance Criteria

1. `CREATE TABLE` with inline `UNIQUE` and `UNIQUE(col, ...)` constraints works.
2. `ALTER TABLE ADD UNIQUE INDEX name (col, ...)` works.
3. `DROP INDEX name ON table` works.
4. INSERT that violates a unique constraint returns a duplicate key error.
5. UPDATE that would create a duplicate under a unique constraint returns an error.
6. NULL values in unique columns do not conflict (MySQL semantics: NULL != NULL).
7. Collated string columns use weight strings in index keys (consistent with PK encoding).
8. Unique indexes are persisted — they survive close/reopen in both journal and native-git modes.
9. Unique indexes survive merge (rebuilt from merged data).
10. `GetIndexes()` returns unique indexes alongside the primary index, and unique-index lookups work through `LookupPartitions`.
11. Composite unique constraints (multiple columns) work.
12. Column alterations (ADD, DROP, MODIFY with reorder or type change) correctly adjust index ordinals.
13. PK-changing updates correctly update the index value (which stores the PK).
14. Failed statements roll back index edits alongside row edits, preserving the nil `idxEdits` state on initialization failure.
15. Journal-mode DDL (ADD/DROP INDEX) does not discard existing table data.
16. Journal checkpoints materialize index trees alongside data trees, including schema-only changes.
17. Rejected type conversions do not corrupt the shared metadata cache.
18. Type changes on indexed columns invalidate the old index tree root and rebuild.
19. Index initialization errors propagate to all editor operations (Insert, Update, Delete).
20. All existing tests continue to pass.

## Flow

### 1. Schema persistence — `indexDisk` and `schemaDisk` (catalog.go)

`indexDisk` struct: `Name string`, `Columns []int` (ordinals), `Unique bool`. Added to `schemaDisk.Indexes` with `omitempty` for backwards compat.

`encodeSchema` / `decodeSchema` signatures extended with `[]indexDisk` parameter/return.

### 2. Manifest persistence — `repository.Table` (repository.go)

`Table` gains `Indexes map[string]storage.Hash` with `omitempty`. `Table.Equal()` method added since the map makes struct non-comparable.

### 3. Index key encoding — `encodeIndexKey` (catalog.go)

Same binary format as `encodeKey` with NULL handling: NULL columns get a `0x00` marker, non-NULL get `0x01` + type-tagged value. When any column is NULL, the PK key is appended as a disambiguator so multiple NULLs produce distinct keys. String columns with non-default collation use weight strings.

### 4. Index state and metadata isolation (catalog.go)

`tableState` gains `indexes []indexDisk` and `idxEdits map[string][]prolly.Edit`. The `cachedTableMeta` struct also gains `indexes`.

**Metadata cloning**: Both cache-hit and cache-miss paths deep-clone all mutable metadata into independent copies. On cache hit, the transaction gets cloned copies via `copySchema`, `copyIndexes`, `copyManifestIndexes`. On cache miss, the cache itself gets cloned copies so the transaction's mutations (DDL, type changes, index renames) cannot leak into the cache. This prevents uncommitted state from being visible to concurrent or subsequent transactions.

### 5. Journal-path support — `ensureIndexEdits` (catalog.go)

When the index tree hasn't been persisted yet (journal path before checkpoint), `ensureIndexEdits` rebuilds in-memory index edits from existing rows. Called lazily before the first uniqueness check. The nil `idxEdits` state is the rebuild signal — it must be preserved when initialization fails.

### 6. Uniqueness enforcement — `editor.Insert/Update/Delete` (catalog.go)

All three operations check `idxInitErr` at the top and return it immediately if set, preventing any bypass of uniqueness enforcement after a failed initialization.

**Insert**: After PK duplicate check, encodes index key for each unique index. Skips uniqueness check when any indexed column is NULL. Otherwise checks pending edits then persisted tree. Appends insert edit.

**Update**: Handles three cases: (a) index key unchanged + PK unchanged → skip, (b) index key unchanged + PK changed → update value only (stores PK), (c) index key changed → delete old + uniqueness check + insert new.

**Delete**: Appends delete edits for all indexes.

**Statement rollback**: `StatementBegin` calls `ensureIndexEdits` atomically — on failure, it stores the error in `idxInitErr` and returns without building the snapshot, preserving the nil `idxEdits` state. On success, it snapshots `idxEdits`. `DiscardChanges` restores the snapshot only if one was taken (non-nil `idxSnapshot`), so a failed initialization preserves nil and allows future retry.

### 7. Commit path — native-git (catalog.go)

`commitIndexTrees` builds or applies index trees at commit time. `coalesceEdits` deduplicates the edit slice (keeps last edit per key) before `prolly.Apply` — handles valid sequences like delete+insert of the same unique value. `buildIndexTreeForDef` builds a single index tree from all rows. Unchanged tables include index tree hashes in the reachable set via `ValidateSnapshot` cache.

### 8. Commit path — journal checkpoint (engine.go)

`checkpointIndexTrees` decodes the schema to get index definitions, then calls `rebuildIndexesFromTree` to build index trees from the checkpointed data tree. Both existing-data and new-table branches include index roots in the table descriptor. Schema objects are copied to the checkpoint writer whenever the schema hash differs from the base (not just for new tables). Schema-only changes (no row edits) are detected and handled as a separate checkpoint branch that copies the schema, retains the data tree, and rebuilds index trees.

`PendingEdits()` (working.go) includes tables with schema data even when they have no row edits, so schema-only changes route through `checkpointTypedEdits`.

### 9. Snapshot validation (engine.go)

`ValidateSnapshot` includes index tree hashes in the per-table object cache, so `commitNativeGit`'s retention logic doesn't discard them for unchanged tables.

### 10. Journal-mode DDL (working.go)

Schema-only typed edits preserve the existing `DataRoot` and `Indexes` by reading and updating the existing table descriptor instead of replacing it.

### 11. DDL — `sql.IndexAlterableTable` (catalog.go)

**CreateIndex**: Validates uniqueness against existing rows, adds `indexDisk`, marks schema dirty.
**DropIndex**: Removes from `indexes`, clears `idxEdits`.
**RenameIndex**: Updates name in `indexes` and re-keys `idxEdits`.

### 12. Column alteration safety (catalog.go)

**AddColumn**: Shifts index ordinals ≥ insert position up by 1. Clears `idxEdits`.
**DropColumn**: Rejects dropping a column used by any index. Shifts ordinals > drop position down by 1. Clears `idxEdits`.
**ModifyColumn with reorder**: Applies the same ordinal remapping as PK ordinals. Clears `idxEdits`.
**ModifyColumn with type change**: Converts values into a temporary map and builds a candidate schema to check for collapsed uniqueness violations *before* mutating shared state. Only applies conversions and installs the new column type if validation passes. Clears `idxEdits` and invalidates `manifest.Indexes` entries for affected indexes so `commitIndexTrees` rebuilds them instead of reusing the old root.

### 13. `GetIndexes` and `LookupPartitions` (catalog.go)

`GetIndexes` returns `uniqueIndex` entries alongside the primary index. `LookupPartitions` dispatches to `lookupPrimaryPartitions` or `lookupUniquePartitions` based on `lookup.Index` type. Unique-index lookups encode the index key, resolve to a PK via `resolveIndexToPK`, then look up the row.

### 14. Merge path — index rebuild (merge.go)

`rebuildIndexesFromTree` scans the merged data tree, builds index entries per index, checks for uniqueness violations in the merged result. `copyTable` includes index tree objects in the reachable set.

### 15. Tests

23 tests covering:
- DDL (CREATE TABLE UNIQUE, ALTER TABLE ADD UNIQUE, DROP INDEX)
- Enforcement (INSERT/UPDATE duplicates, NULL semantics, composite, collation)
- Persistence (journal and native-git modes, close/reopen)
- Native-git multi-table (unchanged table index trees retained)
- PK-changing update (index value updated)
- Column alterations (ADD COLUMN FIRST, DROP indexed column rejected, MODIFY type collapse)
- Delete-then-insert same value (edit coalescing)
- Statement rollback in explicit transaction (failed insert, pre-existing duplicate still rejected)
- Journal checkpoint with DDL (explicit Checkpoint calls, native-git reopen verification)
- Rejected type change does not corrupt metadata cache
- Successful type change rebuilds index and enforces uniqueness

## Decisions

- **One prolly tree per constraint** — same structure as a secondary index. Carries forward to the secondary indexes epic.
- **Rebuild indexes on merge** — simpler and guarantees consistency.
- **Expose unique indexes to the query planner** — free once the trees exist.
- **NULL != NULL (MySQL semantics)** — disambiguated by appending PK bytes.
- **Weight strings for collated keys** — consistent with PK encoding.
- **Coalesce edits before Apply** — handles delete+insert of the same unique value.
- **ensureIndexEdits for journal path** — rebuilds from rows when tree not persisted. Nil is the rebuild signal; preserved on failure.
- **Reject dropping indexed columns** — prevents silent ordinal corruption.
- **Checkpoint builds index trees** — journal path materializes indexes at checkpoint.
- **Validate before mutate on type changes** — prevents cache corruption from rejected ALTER TABLE.
- **Deep-clone all mutable metadata** — both cache-hit and cache-miss paths produce independent copies for the transaction and cache, preventing cross-transaction mutation leaks.
- **Atomic index initialization** — `StatementBegin` either fully initializes the index state or preserves nil; errors propagate via `idxInitErr` to all editor operations.