# Walkthrough: Support ENUM type

## What was built

Native ENUM type support in RepoDB's type system. ENUM columns restrict values to a declared set of strings, stored internally as 1-based `uint16` indices (matching go-mysql-server's representation). The API returns string values at the query boundary, consistent with MySQL wire protocol behavior.

## How the pieces fit together

### Storage layer (`catalog.go`)

The `columnDisk` struct gained an `EnumValues []string` field (JSON `"enum_values,omitempty"`). This is backward-compatible — existing schemas without the field decode to `nil`.

`encodeSchema()` extracts values via `sql.EnumType.Values()`. `decodeSchema()` passes them to `decodeType()`, which calls `types.CreateEnumType(enumValues, sql.Collation_Default)` to reconstruct the type.

The `decodeType()` signature was extended with an `enumValues []string` parameter. All three call sites (`decodeSchema`, `validateSchema`, and the function itself) were updated. Non-enum callers pass `nil`.

`rawValue()` serializes the `uint16` index as a decimal string. `decodeRow()` parses it back and runs it through `Type.Convert()` to get the validated `uint16`.

### Query result conversion (`engine.go`)

go-mysql-server uses `uint16` as the internal runtime type for enums. This is correct for engine operations (ORDER BY sorts by index, not alphabetically), but returning raw `uint16` to API consumers would be surprising. `Session.Query()` now resolves enum indices to strings via `EnumType.At()` before appending rows to the result. This is the same boundary where MySQL's wire protocol performs the conversion.

### ALTER TABLE safety (`catalog.go`)

`ModifyColumn()` converts existing row values to the new column type. For enum-to-enum changes, the old value is a `uint16` index into the *old* enum's value list. Passing that index directly to the new enum's `Convert()` would silently remap values if the list was reordered (e.g., changing `ENUM('red','green')` to `ENUM('green','red')` would turn stored `red` into `green`).

The fix resolves the old `uint16` index to its string value via the old `EnumType.At()` before passing it to the new type's `Convert()`. This preserves string identity through reordering and correctly rejects values removed from the new definition.

### Collation guard (`catalog.go`)

`validateSchema()` rejects ENUM columns with non-default collation. RepoDB does not persist collation for any type today (strings also hardcode `sql.Collation_Default`). Rather than silently discarding an explicit collation — which would change equality/conversion behavior after engine reopen — we fail fast. A future collation story will add collation persistence for all types and lift this restriction.

## Key decisions

1. **uint16→string conversion at the API boundary, not in `decodeRow()`**: Keeping `uint16` in the storage/engine layer preserves correct ORDER BY behavior (definition order, not alphabetical). Converting only in `Query()` mirrors MySQL's wire protocol approach.

2. **Reject non-default collation rather than persist it**: Consistent with how RepoDB handles all types today. Avoids a one-off collation field that would be superseded by the upcoming collation story.

3. **ALTER TABLE resolves old enum index to string before converting**: Without this, reordering enum values silently corrupts stored data — a correctness bug that would be difficult to diagnose.
