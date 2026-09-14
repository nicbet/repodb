# Support DATE, TIME, DATETIME, and TIMESTAMP types

## What

Add temporal SQL types (DATE, TIME, DATETIME, TIMESTAMP) to RepoDB's type system. These types are currently rejected by `validateSchema()` → `decodeType()` with "unsupported M2 SQL type".

## Why

Temporal types are fundamental to real-world schemas. Any consumer storing events, logs, schedules, or timestamps currently has to use TEXT or BIGINT workarounds, losing type safety, MySQL datetime functions, and proper comparison/ordering semantics.

## How

### 1. Schema persistence — `columnDisk` (catalog.go)

Add a `Precision` field to `columnDisk`:

```go
type columnDisk struct {
    Name         string `json:"name"`
    Type         int32  `json:"type"`
    Length       int64  `json:"length,omitempty"`
    Precision    int    `json:"precision,omitempty"`
    Nullable     bool   `json:"nullable"`
    Default      string `json:"default,omitempty"`
    DefaultLit   bool   `json:"default_literal,omitempty"`
    DefaultParen bool   `json:"default_paren,omitempty"`
}
```

`omitempty` ensures backward compatibility — existing schemas without precision decode with zero value (which is the default precision for all temporal types except DATETIME/TIMESTAMP with fractional seconds).

In `encodeSchema`, extract precision from `sql.DatetimeType` when the column is a temporal type.

### 2. `decodeType()` — new cases (catalog.go)

```
Type_DATE      → types.Date (precision 0)
Type_DATETIME  → types.CreateDatetimeType(sqltypes.Datetime, precision)
Type_TIMESTAMP → types.CreateDatetimeType(sqltypes.Timestamp, precision)
Type_TIME      → types.Time (fixed precision 6 in go-mysql-server)
```

DATE, DATETIME, TIMESTAMP are all created via `CreateDatetimeType`. TIME is the singleton `types.Time` (`TimespanType_`).

### 3. Storage — `rawValue()` and `decodeRow()` (catalog.go)

**rawValue**: DATE/DATETIME/TIMESTAMP values are `time.Time` — serialize via `time.Format(time.RFC3339Nano)`. TIME values are `types.Timespan` (named int64, microseconds) — serialize via `fmt.Sprint(int64(value))`.

Add new cases to rawValue:
```
Type_DATE, Type_DATETIME, Type_TIMESTAMP → cast to time.Time, format RFC3339Nano
Type_TIME → cast to types.Timespan, format as int64 string
```

**decodeRow**: Reverse the encoding.
```
Type_DATE, Type_DATETIME, Type_TIMESTAMP → time.Parse(time.RFC3339Nano, string(raw)), then Convert through the column type to truncate precision
Type_TIME → strconv.ParseInt → types.Timespan
```

### 4. `encodeKey()` (catalog.go)

No changes needed. `encodeKey` calls `rawValue()` which will now handle temporal types. The primary index only supports point lookups (not range scans), so the byte representation just needs to be deterministic and unique — RFC3339Nano and int64 string both satisfy this.

### 5. `sqlLiteral()` — parameter binding (engine.go)

Add a `time.Time` case that formats as a MySQL datetime literal:
```go
case time.Time:
    return "'" + value.Format("2006-01-02 15:04:05.999999") + "'", nil
```

No `Timespan` case needed — callers pass TIME values as strings (e.g., `"12:30:00"`), which already work via the string case.

### 6. Update existing tests

Two tests explicitly assert that DATE is rejected:
- `TestAlterTableAddUnsupportedTypeRejected` — change from DATE to a type that will remain unsupported (e.g., YEAR, GEOMETRY, or SET)
- `TestAlterTableModifyToUnsupportedTypeRejected` — same change

### 7. New tests

- **DDL**: CREATE TABLE with each temporal type (DATE, TIME, DATETIME, TIMESTAMP, DATETIME(3))
- **Insert/select round-trip**: Insert temporal values, read them back, verify exact equality
- **Ordering**: ORDER BY on temporal columns
- **Parameter binding**: Insert via `?` parameter with `time.Time` values
- **Persistence**: Close and reopen engine, verify temporal data survives
- **Primary key**: Temporal column as part of a composite primary key
- **NULL handling**: Nullable temporal columns with NULL values
- **DEFAULT**: Temporal column with DEFAULT value

## Acceptance Criteria

1. `CREATE TABLE t (id INT PRIMARY KEY, d DATE, t TIME, dt DATETIME, ts TIMESTAMP)` succeeds
2. Temporal values round-trip through insert → select with exact equality
3. `ORDER BY` on temporal columns produces chronological order
4. `time.Time` works as a `?` parameter
5. Temporal data persists through engine close/reopen (both journal and native-git modes)
6. Temporal columns work as primary key components
7. Existing tests pass (including updated unsupported-type tests)
