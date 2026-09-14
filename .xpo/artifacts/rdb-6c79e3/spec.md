# Support DECIMAL and NUMERIC types

## What

Add fixed-point DECIMAL/NUMERIC types to RepoDB's type system. These are currently rejected by `validateSchema()`.

## Why

FLOAT/DOUBLE lose precision for financial and measurement data. DECIMAL(p,s) provides exact fixed-point arithmetic — essential for prices, quantities, scores, and any domain where `0.1 + 0.2 == 0.3` must hold.

## How

### 1. Schema persistence — `columnDisk` (catalog.go)

Add a `Scale` field:

```go
type columnDisk struct {
    ...
    Precision    int    `json:"precision,omitempty"`
    Scale        int    `json:"scale,omitempty"`
    ...
}
```

`omitempty` ensures backward compatibility. In `encodeSchema`, extract precision and scale from `sql.DecimalType`. In `decodeSchema`, pass scale to `decodeType`.

Note: `Precision` is already used by datetime types (0-6 fractional seconds). For DECIMAL, precision means total digits (1-65) and scale means digits after the decimal (0-30). The field is overloaded by name but the semantics are distinguished by the type code.

### 2. `decodeType()` — new case (catalog.go)

```
Type_DECIMAL → types.CreateColumnDecimalType(uint8(precision), uint8(scale))
```

Default precision=10, scale=0 when both are zero (MySQL default for bare `DECIMAL`).

### 3. Storage — `rawValue()` and `decodeRow()` (catalog.go)

**rawValue**: `decimal.Decimal` values serialize via `value.String()` which produces an exact string representation (e.g. `"123.45"`).

**decodeRow**: Parse the string back via `decimal.NewFromString()`, then `Convert()` through the column type to apply bounds/rounding.

### 4. `sqlLiteral()` — parameter binding (engine.go)

Add a `decimal.Decimal` case: `value.String()` produces a bare numeric literal (no quotes needed).

### 5. `decodeType` signature

Add `scale int` parameter alongside the existing `length` and `precision`. Update all call sites (`decodeSchema`, `validateSchema`).

### 6. Tests

- **DDL**: CREATE TABLE with DECIMAL, DECIMAL(10,2), NUMERIC
- **Round-trip**: Insert exact decimal values, verify they survive without precision loss
- **Arithmetic**: `SELECT price * quantity` produces exact result
- **Ordering**: ORDER BY on DECIMAL column
- **Bounds**: Values exceeding precision/scale are rejected
- **Persistence**: Close and reopen engine
- **NULL handling**: Nullable DECIMAL column

## Acceptance Criteria

1. `CREATE TABLE t (id INT PRIMARY KEY, price DECIMAL(10,2))` succeeds
2. `99.99` round-trips exactly (not `99.98999...`)
3. Arithmetic on DECIMAL columns produces exact results
4. Values exceeding declared precision/scale are handled correctly
5. DECIMAL data persists through engine close/reopen
