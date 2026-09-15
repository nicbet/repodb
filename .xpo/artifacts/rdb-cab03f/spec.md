## What

Add a persisted collation field to string (and enum) columns so that collation-aware comparisons work correctly. At minimum, support `utf8mb4_general_ci` for case-insensitive matching alongside the existing default (`utf8mb4_0900_bin`). This unlocks case-insensitive WHERE, ORDER BY, and UNIQUE constraint behavior on string columns.

## Why

RepoDB currently hardcodes `sql.Collation_Default` (`utf8mb4_0900_bin`) everywhere. String columns created with an explicit collation (e.g. `CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci`) silently lose the collation — it's not persisted to disk and is reconstructed as the default on read. This makes case-insensitive queries impossible and is the last child issue under the "Expand supported SQL types and collations" epic (`rdb-e45ce6`).

## Acceptance Criteria

1. A `CREATE TABLE` with `COLLATE utf8mb4_general_ci` on a VARCHAR/CHAR/TEXT column persists the collation and reconstructs it correctly on reopen.
2. `WHERE name = 'alice'` matches a row stored as `'Alice'` when the column uses `utf8mb4_general_ci`.
3. `ORDER BY` on a `utf8mb4_general_ci` column sorts case-insensitively (e.g. `'alice' < 'Bob'`).
4. ENUM columns also support non-default collation (remove the current rejection in `validateSchema`).
5. Existing databases with no collation field in their schema JSON decode correctly (backwards compatibility — absent field → `Collation_Default`).
6. `table.Collation()` continues to return `Collation_Default` (table-level collation is out of scope — column-level is sufficient).
7. Collated string primary keys use collation-aware key encoding — inserting "alice" when "Alice" exists must be rejected as a duplicate under `utf8mb4_general_ci`.
8. All existing tests continue to pass.

## Flow

### 1. Add `Collation` field to `columnDisk` (catalog.go:1168)

```go
Collation  string   `json:"collation,omitempty"`
```

Store as a string name (e.g. `"utf8mb4_general_ci"`). The `omitempty` tag ensures existing schemas without collation decode with an empty string, which we map to `Collation_Default`. String storage is the industry convention for human-readable formats (JSON, information_schema) — a reader inspecting stored data can immediately identify the collation without a numeric lookup table.

### 2. Extract collation in `encodeSchema()` (catalog.go:1185)

After the existing `st.Length()` extraction for string types, also extract collation:

```go
if st, ok := col.Type.(sql.StringType); ok {
    cd.Length = st.Length()
    if c := st.Collation(); c != sql.Collation_Default {
        cd.Collation = c.Name()
    }
}
```

Also extract collation from ENUM types:

```go
if et, ok := col.Type.(sql.EnumType); ok {
    cd.EnumValues = et.Values()
    if c := et.Collation(); c != sql.Collation_Default {
        cd.Collation = c.Name()
    }
}
```

### 3. Pass collation through `decodeType()` (catalog.go:1241)

Add a `collation sql.CollationID` parameter to `decodeType`. Update the string and ENUM cases to use it instead of hardcoding `sql.Collation_Default`.

### 4. Update `decodeSchema()` call site (catalog.go:1218)

Resolve collation from `cd.Collation` — empty string maps to `sql.Collation_Default`, otherwise parse with `sql.ParseCollation("", cd.Collation, false)` — and pass to `decodeType`.

### 5. Update `validateSchema()` (catalog.go:1296)

- Extract collation from string types for the round-trip validation.
- **Remove** the ENUM non-default collation rejection (lines 1317-1318).
- Pass collation into `decodeType` call.

### 6. Collation-aware primary key encoding (`encodeKey`, catalog.go:1458)

For string PK columns with a non-default collation, `encodeKey` must use the collation's weight string instead of raw bytes. Use `CollationID.WriteWeightString()` to produce canonical key bytes — two strings equivalent under the collation (e.g. "Alice" and "alice" under `utf8mb4_general_ci`) produce identical keys.

Check if the PK column's `sql.Type` is a `sql.StringType` with a non-default collation. If so, write weight string bytes instead of raw string bytes.

### 7. Tests

- **Round-trip test**: Create a table with a `utf8mb4_general_ci` VARCHAR column, insert rows, close/reopen, verify column collation is preserved.
- **Case-insensitive WHERE**: Insert `'Alice'`, query `WHERE col = 'alice'`, expect match.
- **Case-insensitive ORDER BY**: Insert mixed-case values, verify sort order ignores case.
- **ENUM with collation**: Create an ENUM column with `utf8mb4_general_ci`, verify it's accepted and round-trips.
- **Backwards compatibility**: Decode a schema JSON blob without the `collation` field, verify it defaults to `Collation_Default`.
- **Collated string PK uniqueness**: Use a collated VARCHAR as primary key, verify that inserting case-variants is rejected as duplicate.
- **Collated string PK lookup**: Use a collated VARCHAR as primary key, verify WHERE on PK matches case-insensitively.

## Decisions

- **Store collation as string name, not uint16** — JSON is a human-readable format; `"collation": "utf8mb4_general_ci"` is self-documenting while `"collation": 45` requires a lookup table. This matches the convention used by MySQL's `information_schema`, SQLite, and PostgreSQL for metadata storage. The decode cost of `sql.ParseCollation` is one map lookup — trivial.
- **`omitempty` for backwards compatibility** — empty string means "not specified", which maps to `Collation_Default`. Existing schema blobs missing the field decode correctly without migration.
- **Column-level only, not table-level** — `table.Collation()` stays as `Collation_Default`. Table-level collation in MySQL is just a DDL default for new columns, not a comparison semantic. The column type carries the actual collation used for comparisons. If `ALTER TABLE ADD COLUMN` support arrives later, that's when table-level collation would matter — and GMS resolves the default before handing the column to RepoDB anyway.
- **No allowlist of supported collations** — go-mysql-server handles the collation comparison logic. Any `CollationID` it supports should work. If a collation's sorter isn't compiled, GMS returns an error at the right level.
- **Weight strings for PK keys** — `CollationID.WriteWeightString()` produces the canonical byte sequence for a string under its collation. Two collation-equivalent strings produce identical weight bytes, ensuring PK uniqueness and correct lookups. This is the same approach MySQL uses for index keys.

## Edge Cases

- **HIGH**: Collated string primary keys — `encodeKey` must use weight strings, not raw bytes, for PK columns with non-default collation. Without this, duplicate case-variants slip through and lookups fail.
- **HIGH**: Backwards compatibility of existing schema blobs — tested by decoding a blob without the field. Empty string maps to default.
- **MEDIUM**: Collation mismatch in comparisons across columns (e.g. JOIN with different collations) — handled by GMS's coercibility rules, not RepoDB's concern.
- **LOW**: Binary collation on string types — already works via `Collation_binary`, same code path.