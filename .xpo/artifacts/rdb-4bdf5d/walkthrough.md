# Non-unique secondary index support

## What was built

RepoDB now supports non-unique secondary indexes (`CREATE INDEX`, `DROP INDEX`) with full query planner integration. These accelerate equality lookups without enforcing uniqueness.

## How the pieces fit together

### Key encoding change

The core design decision: for non-unique indexes, the PK is **always** appended to the index key, not just when a column is NULL. This ensures each row gets a distinct entry in the prolly tree even when indexed column values are identical.

`encodeIndexKey` gained a `unique bool` parameter. The suffix rule is `if hasNull || !unique { append PK }`. For lookups, callers encode just the column values (pass `nil` PK, `unique=true`) to get the prefix for scanning.

### Index type generalization

`uniqueIndex` → `secondaryIndex`. The only behavioral change is `IsUnique()` now returns `def.Unique` instead of hardcoded `true`. This lets the query planner correctly classify non-unique indexes.

### Write path

`Insert` previously skipped non-unique indexes entirely (`if !idx.Unique { continue }`). Now it maintains all indexes — encoding the key with the appropriate uniqueness flag and only performing the duplicate check when `idx.Unique` is true.

`Delete` and `Update` already looped over all indexes without skipping; they just needed the `idx.Unique` parameter passed through to `encodeIndexKey`.

### Lookup path

New `lookupSecondaryPartitions`: encodes the lookup columns as a prefix (column values only, no PK), then calls `resolveIndexToPKs`.

New `resolveIndexToPKs`: uses `IteratorFrom(prefix)` on the persisted index tree to find all entries whose key starts with the prefix, then merges pending edits on top (edits with matching prefix override or delete tree entries). Returns all surviving PKs.

`LookupPartitions` dispatches `secondaryIndex` to either `lookupUniquePartitions` (exact key match, single PK) or `lookupSecondaryPartitions` (prefix scan, multiple PKs) based on `def.Unique`.

### Merge path

`rebuildIndexesFromTree` already handled non-unique indexes correctly — it only checks for duplicate keys when `idx.Unique` is true. With the key encoding change (PK always appended for non-unique), rebuilt keys are naturally unique per entry. No structural changes needed, just the `idx.Unique` parameter threaded through.

## Key decisions

- **PK always in key (not in value)** for non-unique indexes. Alternative was multi-PK values per key (avoiding prefix scan), but that would require read-modify-write on every insert and complex edit coalescing. PK-in-key with prefix scan is cleaner and aligns with how real databases work.

- **Prefix matching via `bytes.HasPrefix`** is safe because the column encoding is self-delimiting (null flag + type bytes + 4-byte length prefix for each column value). A shorter column value cannot be a false prefix of a longer one.

- **`resolveIndexToPKs` applies edits in order** rather than scanning backwards for "last edit wins" per full key. It builds a map from the tree, then overlays all edits (later edits overwrite earlier ones for the same full key). This is correct because the map naturally deduplicates.

## Files changed

| File | Change |
|------|--------|
| `engine/catalog.go` | `encodeIndexKey` +unique param, `CreateIndex` non-unique gate removed, `uniqueIndex`→`secondaryIndex`, `Insert` maintains all indexes, new `lookupSecondaryPartitions`/`resolveIndexToPKs`, `LookupPartitions` dispatch |
| `engine/merge.go` | `encodeIndexKey` call updated with `idx.Unique` |
| `engine/engine_test.go` | 12 new tests: DDL, duplicates, delete, update, nulls, composite, persistence, drop, alter-table-add, native-git, native-git persistence, coexistence with unique |
| `engine/merge_performance_test.go` | 1 new test: three-way merge rebuilds non-unique index tree correctly |
