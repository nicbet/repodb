# Support ENUM type

## What

Add ENUM type to RepoDB's type system. ENUM columns restrict values to a fixed set of strings, stored internally as 1-based `uint16` indices.

## Why

ENUM is a common MySQL type for columns with a small, fixed set of values (status codes, categories, roles). Without native support, consumers must use VARCHAR with application-level validation, losing type safety and storage efficiency.

## How

### 1. Schema persistence — `columnDisk` (catalog.go)

Add an `EnumValues` field:

```go
type columnDisk struct {
    // ... existing fields ...
    EnumValues []string `json:"enum_values,omitempty"`
}
```

Backward compatible: existing schemas without the field decode to `nil`.

### 2. `encodeSchema()` (catalog.go)

Add a type-assertion branch for `sql.EnumType` to extract the allowed values:

```go
if et, ok := col.Type.(sql.EnumType); ok {
    cd.EnumValues = et.Values()
}
```

### 3. `decodeType()` signature change (catalog.go)

Extend the signature to accept enum values:

```go
func decodeType(t querypb.Type, length int64, precision int, scale int, enumValues []string) (sql.Type, error)
```

Add the ENUM case:

```go
case querypb.Type_ENUM:
    return types.CreateEnumType(enumValues, sql.Collation_Default)
```

### 4. `decodeSchema()` (catalog.go)

Pass `cd.EnumValues` to the updated `decodeType()` call.

### 5. `validateSchema()` — enum values and collation guard (catalog.go)

Add an extraction branch for `sql.EnumType` to get enum values, then pass them to `decodeType()`.

Reject non-default collations with an explicit error. RepoDB does not persist collation for any type today (strings also hardcode `sql.Collation_Default`). Rather than silently discarding an explicit collation — which would change equality and conversion behavior after reopen — we fail fast. A future collation story will add collation persistence for all types and lift this restriction.

### 6. `rawValue()` (catalog.go)

Add a case for `querypb.Type_ENUM`. The runtime value is `uint16`:

```go
case querypb.Type_ENUM:
    v, ok := value.(uint16)
    if !ok {
        return nil, fmt.Errorf("enum value has type %T", value)
    }
    return []byte(strconv.FormatUint(uint64(v), 10)), nil
```

### 7. `decodeRow()` (catalog.go)

Add a case for `querypb.Type_ENUM`. Parse the stored uint16 index and convert:

```go
case querypb.Type_ENUM:
    v, err := strconv.ParseUint(string(raw), 10, 16)
    if err != nil {
        return nil, err
    }
    row[i], _, err = schema[i].Type.Convert(context.Background(), v)
    if err != nil {
        return nil, err
    }
```

### 8. `Query()` result conversion (engine.go)

Convert enum uint16 indices to strings at the API boundary in `Session.Query()` using `EnumType.At()`. This keeps storage and engine internals using uint16 (correct for sorting/comparisons) and only converts at the API boundary, matching MySQL wire protocol behavior.

### 9. ALTER TABLE MODIFY COLUMN — enum index resolution (catalog.go)

The existing MODIFY COLUMN path calls `column.Type.Convert(ctx, row[colIdx])` directly. For enum-to-enum changes where the value list is reordered, this passes the old uint16 index to the new type — mapping to a wrong string.

Fix: before converting, resolve the old enum index to its string value using the old type's `At()` method, then pass the string to the new type's `Convert()`.

### 10. `sqlLiteral()` (engine.go)

No changes needed — `uint16` already has a case that formats as a number. ENUM default values stored as string literals go through the existing string path.

## Flow

1. Add `EnumValues` to `columnDisk`
2. Update `decodeType()` signature (add `enumValues []string`), add ENUM case
3. Update all `decodeType()` callers: `decodeSchema()`, `validateSchema()`
4. Update `encodeSchema()` to extract enum values
5. Reject non-default collation in `validateSchema()`
6. Add `rawValue()` case for ENUM
7. Add `decodeRow()` case for ENUM
8. Add enum→string conversion in `Query()`
9. Fix ALTER TABLE MODIFY COLUMN for enum index resolution
10. Write tests

## Acceptance Criteria

- [ ] `CREATE TABLE` with ENUM column succeeds
- [ ] `SHOW CREATE TABLE` round-trips the ENUM definition with correct values
- [ ] Insert valid enum values (by string) succeeds and round-trips through select
- [ ] Insert invalid enum value is rejected
- [ ] ENUM column with NULL works (nullable)
- [ ] ENUM column with DEFAULT value works
- [ ] ENUM values persist through engine reopen (close + reopen)
- [ ] ORDER BY on ENUM column orders by index (definition order), not alphabetically
- [ ] ALTER TABLE MODIFY COLUMN with reordered ENUM values preserves string identity
- [ ] Non-default collation on ENUM is rejected (collation persistence deferred to collation story)
- [ ] All existing tests continue to pass
