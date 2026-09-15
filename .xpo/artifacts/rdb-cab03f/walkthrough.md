## Summary

Added collation persistence and collation-aware key encoding to RepoDB. String and ENUM columns now preserve their collation through schema serialization, enabling case-insensitive WHERE, ORDER BY, and primary key uniqueness semantics.

## What Changed

All changes are in `engine/catalog.go` and `engine/engine_test.go`.

### Schema persistence (`columnDisk`, `encodeSchema`, `decodeSchema`)

`columnDisk` gains a `Collation string` field (`json:"collation,omitempty"`). The collation is stored as a human-readable name (e.g. `"utf8mb4_general_ci"`) rather than a numeric ID — this matches the convention used by MySQL's `information_schema` and keeps the stored JSON self-documenting.

`encodeSchema` extracts the collation from `sql.StringType` and `sql.EnumType` columns. Only non-default collations are stored; the default (`utf8mb4_0900_bin`) is omitted via `omitempty`, which also provides backwards compatibility — existing schema blobs without the field decode correctly since the empty string maps to `Collation_Default`.

`decodeSchema` resolves the collation string back to a `sql.CollationID` via `sql.ParseCollation` and passes it through to `decodeType`.

### Type reconstruction (`decodeType`)

`decodeType` gains a `collation sql.CollationID` parameter. The VARCHAR/CHAR/TEXT and ENUM cases now use this parameter instead of hardcoding `sql.Collation_Default`. All other type cases ignore it.

### Schema validation (`validateSchema`)

The ENUM non-default collation rejection (previously added as a guard in rdb-ef5811) is removed — it's no longer needed now that collation is persisted. Both string and enum type collations are extracted and passed through to the round-trip validation via `decodeType`.

### Primary key encoding (`encodeKey`)

This was the critical correctness fix identified during review. `encodeKey` previously stored raw string bytes for all string PK columns, meaning "Alice" and "alice" produced different keys even under a case-insensitive collation.

The fix: for string PK columns with a non-default collation, `encodeKey` calls `CollationID.WriteWeightString()` to produce the collation's canonical byte representation. Two strings that are equivalent under the collation (e.g. "Alice" and "alice" under `utf8mb4_general_ci`) produce identical weight bytes, correctly enforcing PK uniqueness and enabling collation-aware lookups.

This works because all key operations (insert, lookup, update, delete) flow through `encodeKey`, so the fix is applied consistently.

## Key Decisions

- **String storage over uint16**: The user confirmed this choice after discussion. JSON is a human-readable format; `"collation": "utf8mb4_general_ci"` is immediately understandable while `"collation": 45` requires a lookup table. The decode cost of `sql.ParseCollation` is one map lookup.

- **Column-level only, not table-level**: `table.Collation()` still returns `Collation_Default`. Table-level collation in MySQL is a DDL default for new columns, not a comparison semantic. GMS resolves column collation before handing the schema to RepoDB.

- **No collation allowlist**: Any collation supported by go-mysql-server works. GMS handles the comparison/sorting logic and returns appropriate errors for unsupported collations.

- **Weight strings for PK keys**: Uses `WriteWeightString` (full weight byte sequence) rather than `HashToBytes` (lossy hash). Weight strings are collision-free within a collation's equivalence classes — the same approach MySQL uses for index keys.

## Tests

8 new tests in `engine_test.go`:

1. `TestCollationCaseInsensitiveWhere` — WHERE matches across case under `utf8mb4_general_ci`
2. `TestCollationCaseInsensitiveOrderBy` — ORDER BY sorts case-insensitively
3. `TestCollationPersistsThroughReopen` — collation survives close/reopen, visible in SHOW CREATE TABLE
4. `TestCollationDefaultBackwardsCompat` — default collation remains case-sensitive after reopen
5. `TestCollationStringPKUniqueness` — duplicate case-variant PK rejected
6. `TestCollationStringPKLookup` — PK WHERE matches case-insensitively
7. `TestCollationStringPKPersistsThroughReopen` — PK uniqueness and lookup survive reopen
8. `TestEnumWithCollation` — ENUM with non-default collation accepted and round-trips