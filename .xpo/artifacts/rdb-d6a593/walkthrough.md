# Support DATE, TIME, DATETIME, and TIMESTAMP types

RepoDB now supports temporal SQL types: DATE, TIME, DATETIME, and TIMESTAMP (with optional fractional-second precision up to 6). These were previously rejected by `validateSchema()` as unsupported M2 types.

## How the pieces fit together

The change extends five switch statements across two files — no new files, structs, or interfaces.

### Schema persistence

`columnDisk` gains a `Precision int` field (`json:"precision,omitempty"`). Existing schemas without the field decode with zero precision, which is the default for DATE, DATETIME, and TIMESTAMP — backward compatibility is free. `encodeSchema()` extracts precision from `sql.DatetimeType` columns; `decodeSchema()` passes it through to `decodeType()`.

### Type mapping (`decodeType`)

- `Type_DATE`, `Type_DATETIME`, `Type_TIMESTAMP` → `types.CreateDatetimeType(baseType, precision)` — all three are datetime variants that store `time.Time` values, differing only in range and semantics.
- `Type_TIME` → `types.Time` — the singleton `TimespanType_`, which stores `types.Timespan` (a named `int64` of microseconds).

`validateSchema()` was updated to extract and pass precision alongside length, keeping the round-trip through `decodeType` consistent.

### Storage encoding

**`rawValue()`** serializes temporal values to bytes:
- DATE/DATETIME/TIMESTAMP: `time.Time` → `t.UTC().Format(time.RFC3339Nano)` — UTC normalization ensures the stored instant is timezone-independent.
- TIME: `types.Timespan` → `strconv.FormatInt(int64(ts), 10)` — microseconds as a decimal string.

**`decodeRow()`** reverses the encoding:
- DATE/DATETIME/TIMESTAMP: `time.Parse(time.RFC3339Nano, ...)` then `Convert()` through the column type to truncate to the declared precision.
- TIME: `strconv.ParseInt(...)` → `types.Timespan(v)`.

Primary key encoding (`encodeKey`) required no changes — it delegates to `rawValue()`, and the primary index only supports point lookups, so byte ordering doesn't matter.

### Parameter binding (`sqlLiteral`)

`time.Time` values are formatted as `value.UTC().Format("2006-01-02 15:04:05.999999")` — a MySQL-compatible datetime literal. The `.UTC()` normalization is critical: without it, a `time.Time` in a non-UTC zone would format its local wall-clock time, but the SQL parser would interpret it as UTC, silently shifting the instant. This was caught during review and fixed before merge.

No `Timespan` case is needed — callers pass TIME values as strings.

### Test updates

Two existing tests (`TestAlterTableAddUnsupportedTypeRejected`, `TestAlterTableModifyToUnsupportedTypeRejected`) were changed from DATE to YEAR, since DATE is now supported.

14 new tests cover:
- DDL with all four types, including `DATETIME(3)` and `TIMESTAMP(6)`
- Insert/select round-trip for each type individually
- `ORDER BY` on DATETIME columns (chronological ordering)
- `time.Time` parameter binding (UTC and non-UTC zones)
- Nullable temporal columns with NULL values
- DATETIME as a primary key (uniqueness + point lookup)
- Persistence through engine close/reopen (journal and native-git modes)
- DEFAULT values on DATE columns

## Key decisions

**RFC3339Nano for storage, not MySQL format.** RFC3339Nano preserves full nanosecond precision and is unambiguous. The MySQL datetime format (`2006-01-02 15:04:05.999999`) is used only in `sqlLiteral` where the SQL parser expects it.

**UTC normalization at both storage and binding boundaries.** Both `rawValue()` and `sqlLiteral()` normalize to UTC, ensuring the same instant is stored regardless of the `time.Time`'s location. This is a correctness invariant — the SQL engine operates in UTC.

**No new `columnDisk` fields beyond `Precision`.** The `Length` field is reserved for string types. Adding a dedicated `Precision` field is cleaner than overloading `Length` and self-documents the distinction.
