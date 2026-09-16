# Non-unique secondary index support

## What
Extend the UNIQUE constraint index infrastructure to support non-unique `CREATE INDEX` / `DROP INDEX`, with full query planner integration.

## Why
The UNIQUE work (rdb-a1f9d1) built the entire index pipeline but gates on `def.IsUnique()`. Non-unique indexes accelerate lookups without enforcing uniqueness — they're the most common index type in practice.

## How

### 1. Key encoding — always append PK for non-unique indexes (`catalog.go`)

`encodeIndexKey` gains a `unique bool` parameter. When `unique` is false, PK is always appended as a disambiguator (not just for NULLs). All 12 call sites updated.

### 2. Remove `CreateIndex` unique-only gate (`catalog.go`)

Deleted the `if !def.IsUnique()` rejection. Non-unique indexes created with `Unique: false`. Uniqueness scan only runs for unique indexes.

### 3. Rename `uniqueIndex` → `secondaryIndex` (`catalog.go`)

`IsUnique()` returns `def.Unique` instead of hardcoded `true`. `GetIndexes` returns `secondaryIndex` for all non-primary indexes.

### 4. Insert: maintain non-unique indexes (`catalog.go`)

Removed `if !idx.Unique { continue }`. All indexes now maintained; uniqueness check gated on `idx.Unique`.

### 5. Non-unique index lookup (`catalog.go`)

New `lookupSecondaryPartitions` method: encodes column values as prefix key (via `encodeIndexKey` with nil PK, unique=true), calls `resolveIndexToPKs` to find all matching rows.

New `resolveIndexToPKs`: prefix-scans the persisted index tree via `IteratorFrom`, merges with pending edits (edits override tree entries for matching keys).

### 6. `LookupPartitions` dispatch (`catalog.go`)

Routes `secondaryIndex` to `lookupUniquePartitions` (when unique) or `lookupSecondaryPartitions` (when not).

### 7. Merge path (`merge.go`)

`rebuildIndexesFromTree` updated to pass `idx.Unique` to `encodeIndexKey`. Non-unique indexes produce PK-suffixed keys that are naturally unique per entry, so no duplicate check needed.

## Acceptance Criteria

- [x] `CREATE INDEX idx_name ON t(col)` works (non-unique)
- [x] `DROP INDEX idx_name ON t` works for non-unique indexes
- [x] Non-unique index maintained on INSERT, UPDATE, DELETE
- [x] Query planner uses non-unique index for equality lookups (point queries)
- [x] Multiple rows with same indexed value are all returned
- [x] NULL values handled correctly in non-unique indexes
- [x] Unique indexes continue to work exactly as before
- [x] Merge/sync rebuilds non-unique indexes correctly
- [x] Persistence: non-unique indexes survive close/reopen
