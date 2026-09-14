# Support DECIMAL and NUMERIC types

RepoDB now supports fixed-point DECIMAL and NUMERIC column types with configurable precision and scale. Previously, exact decimal values could only be approximated via FLOAT/DOUBLE (lossy) or stored as TEXT (no arithmetic).

## How the pieces fit together

### Schema persistence

`columnDisk` gains a `Scale int` field (`json:"scale,omitempty"`). The existing `Precision` field (added for datetime fractional seconds) is reused — the type code distinguishes whether precision means "fractional second digits" (datetime) or "total decimal digits" (DECIMAL). `encodeSchema()` extracts both via `sql.DecimalType.Precision()` and `.Scale()`.

### Type mapping (`decodeType`)

`Type_DECIMAL` → `types.CreateColumnDecimalType(precision, scale)`. When both are zero (bare `DECIMAL` with no parameters), defaults to precision=10, scale=0 — matching MySQL's default. `CreateColumnDecimalType` (not `CreateDecimalType`) is used because it enforces column-level bounds checking on insert.

The `decodeType` signature gained a `scale int` parameter. All call sites (`decodeSchema`, `validateSchema`) were updated.

### Storage encoding

**`rawValue()`**: `decimal.Decimal` values serialize via `d.String()`, which produces an exact representation. Note: trailing zeros are trimmed (`0.10` → `0.1`), but precision is preserved — the `Convert()` call on decode restores the column's declared scale.

**`decodeRow()`**: Parse via `decimal.NewFromString()`, then `Convert()` through the column type to apply bounds and rounding.

### Parameter binding (`sqlLiteral`)

`decimal.Decimal` values format as bare numeric literals via `value.String()` — no quotes, since the SQL parser expects unquoted numbers.

### Module metadata

`github.com/shopspring/decimal` was promoted from `indirect` to a direct dependency in `go.mod` via `go mod tidy`, since both `catalog.go` and `engine.go` now import it directly.

### Tests

10 tests with exact value assertions:
- **DDL**: DECIMAL(10,2) and NUMERIC(5,3)
- **Bare DECIMAL**: No precision/scale parameters (defaults to (10,0))
- **Round-trip**: 99.99, 0.1, 12345678.5 — exact string comparison
- **Precision overflow**: 1000.00 into DECIMAL(5,2) rejected (max is 999.99)
- **Scale rounding**: 1.999 into DECIMAL(5,2) rounds to 2.00
- **Arithmetic**: `price * qty` = exact 59.97
- **ORDER BY**: Correct numeric ordering
- **Nullable**: NULL DECIMAL column
- **Persistence**: Values survive engine close/reopen
- **DEFAULT**: DECIMAL column with default value

## Key decisions

**Reusing `Precision` for both datetime and decimal.** The field name is overloaded, but the type code (`Type_DATETIME` vs `Type_DECIMAL`) unambiguously selects the interpretation. Adding a separate `DecimalPrecision` field would be clearer but add schema bloat for no functional benefit.

**`CreateColumnDecimalType` not `CreateDecimalType`.** The "column" variant enforces precision/scale bounds on every `Convert()` call, which is what we want for stored data. The non-column variant is for intermediate arithmetic results where bounds don't apply.

**`decimal.String()` trims trailing zeros.** This means `0.10` stores as `"0.1"`. The round-trip is still exact because `decodeRow` passes the parsed decimal through `Convert()`, which applies the column's scale. Tests assert against the trimmed form to avoid false failures.
