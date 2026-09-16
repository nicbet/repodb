## Summary

Added UNIQUE constraint support to RepoDB, backed by dedicated prolly trees — one per constraint. Each index tree maps constrained column values to the primary key, enabling O(log n) uniqueness enforcement and query-planner-accelerated point lookups. The index tree format carries forward directly into the secondary indexes epic.

## What Changed

### Schema and manifest persistence

`repository.Table` (repository.go) gains `Indexes map[string]storage.Hash` for index tree root persistence, plus `Table.Equal()` since the map makes the struct non-comparable.

`indexDisk` struct added to `schemaDisk` (catalog.go) — stores name, column ordinals, and unique flag. `encodeSchema`/`decodeSchema` signatures extended to carry `[]indexDisk`.

### Index key encoding

`encodeIndexKey` (catalog.go) builds binary keys from arbitrary columns. NULL columns get a `0x00` marker; non-NULL get `0x01` + type-tagged value. When any column is NULL, the PK is appended as a disambiguator (MySQL semantics: NULL != NULL). Collated strings use weight strings, consistent with PK encoding.

### Metadata isolation

Both cache-hit and cache-miss paths in `StartTransaction` deep-clone all mutable metadata (`copySchema`, `copyIndexes`, `copyManifestIndexes`) so transactions and the cache hold independent copies. `loadTableMetadata` also clones `manifest.Indexes` from the snapshot. This prevents DDL operations (RenameIndex, ModifyColumn type changes) from leaking uncommitted state across transactions or corrupting the cache on failure.

### Index state management

`tableState` gains `indexes []indexDisk` and `idxEdits map[string][]prolly.Edit`. The nil state of `idxEdits` is semantically meaningful: nil = "needs rebuild from rows" (journal path before checkpoint), non-nil = "valid, accumulate edits here."

`ensureIndexEdits` rebuilds the map from existing rows when indexes aren't persisted yet. It builds into a local map and assigns atomically on success, preserving nil on failure. When all indexes are persisted (native-git mode), it initializes an empty map without scanning rows.

### Uniqueness enforcement

`editor.Insert`, `Update`, and `Delete` all check `idxInitErr` at the top — if `StatementBegin`'s initialization failed, every operation returns the error rather than proceeding with potentially stale state.

**Insert**: Encodes index key, skips uniqueness check for NULL, otherwise checks pending edits then persisted tree, appends insert edit.

**Update**: Three cases — (a) index key + PK both unchanged → skip, (b) index key unchanged + PK changed → update the stored PK value, (c) index key changed → delete old + uniqueness check + insert new.

**Delete**: Appends delete edits for all indexes.

**Statement rollback**: `StatementBegin` calls `ensureIndexEdits` atomically — on failure, returns early without building a snapshot, preserving nil `idxEdits`. On success, snapshots `idxEdits`. `DiscardChanges` restores the snapshot only if one was taken.

### Commit paths

**Native-git** (`commitNativeGit`): `commitIndexTrees` builds or applies index trees. `coalesceEdits` deduplicates the edit slice (keeps last edit per key) before `prolly.Apply`, handling valid delete+insert sequences on the same unique value. `ValidateSnapshot` includes index tree hashes in the per-table object cache so unchanged tables retain their index objects through `RetainOnly`.

**Journal checkpoint** (`checkpointTypedEdits`): `checkpointIndexTrees` decodes the schema, gets index definitions, and calls `rebuildIndexesFromTree`. Schema-only changes (no row edits) are detected via a separate checkpoint branch that copies the schema object, retains the data tree, and rebuilds index trees. `PendingEdits()` includes tables with schema data even without row edits.

### DDL

`sql.IndexAlterableTable` implemented: `CreateIndex` validates uniqueness against existing rows, `DropIndex` removes the definition and clears edits, `RenameIndex` updates the name.

### Column alteration safety

`AddColumn` and `ModifyColumn` with reorder shift index ordinals with the same logic as PK ordinals. `DropColumn` rejects dropping a column used by any index. `ModifyColumn` with type change converts values into a temporary map and validates uniqueness against a candidate schema *before* mutating state. On success, it invalidates affected `manifest.Indexes` entries to force tree rebuild.

### Query planner integration

`GetIndexes` returns `uniqueIndex` entries alongside the primary index. `LookupPartitions` dispatches to `lookupPrimaryPartitions` or `lookupUniquePartitions` based on `lookup.Index` type. Unique-index lookups encode the index key, resolve to a PK via `resolveIndexToPK`, then look up the row.

### Merge

`rebuildIndexesFromTree` scans the merged data tree, builds index entries per index, and checks for uniqueness violations. `copyTable` includes index tree objects in the reachable set.

## Key Decisions

- **One prolly tree per constraint** — same format as a secondary index. The secondary indexes epic generalizes this without changes.
- **Rebuild indexes on merge** — simpler than three-way merging index trees and guarantees consistency.
- **NULL != NULL** — disambiguated by appending PK bytes to the index key.
- **Coalesce edits before Apply** — handles delete+insert of the same unique value within one transaction.
- **Validate before mutate** on type changes — prevents cache corruption from rejected ALTER TABLE.
- **Deep-clone all mutable metadata** at every sharing boundary — both cache paths, loadTableMetadata, and snapshot manifests.
- **Atomic index initialization** — `ensureIndexEdits` builds into a local map, assigns only on success. `StatementBegin` preserves nil on failure and propagates the error via `idxInitErr` to all editor operations.

## Tests

23 tests covering DDL, enforcement (INSERT/UPDATE duplicates, NULL semantics, composite, collation), persistence (journal and native-git modes), native-git multi-table retention, PK-changing updates, column alterations, edit coalescing, statement rollback in explicit transactions, journal checkpoint with DDL, metadata cache isolation, and indexed type change rebuild.