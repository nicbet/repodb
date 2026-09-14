# Support native JSON type

## What

Add native JSON type to RepoDB's type system. Currently JSON data can only be stored as TEXT, losing JSON validation and JSON function support.

## Why

Native JSON enables `JSON_EXTRACT`, `JSON_OBJECT`, `JSON_ARRAY`, and other MySQL JSON functions that go-mysql-server provides. It also validates JSON on insert, catching malformed data early.

## How

### 1. `decodeType()` — new case (catalog.go)

```
Type_JSON → types.JSON
```

No parameters (no length/precision). The singleton `types.JSON` (`JsonType{}`) handles everything.

### 2. Storage — `rawValue()` and `decodeRow()` (catalog.go)

**rawValue**: JSON values arrive as `sql.JSONWrapper` (typically `types.JSONDocument{Val: any}`). Serialize via `types.JsonToMySqlString()` which produces a canonical MySQL-compatible JSON string.

```go
case querypb.Type_JSON:
    jw, ok := value.(sql.JSONWrapper)
    // serialize to canonical JSON string
```

**decodeRow**: The stored bytes are a JSON string. Pass it through `types.JSON.Convert()` which parses the string into a `JSONDocument`.

```go
case querypb.Type_JSON:
    row[i], _, err = types.JSON.Convert(context.Background(), string(raw))
```

### 3. `sqlLiteral()` — no changes needed (engine.go)

JSON values are passed as strings by callers, which already works through the existing string case.

### 4. Tests

- **DDL**: CREATE TABLE with JSON column
- **Insert/select round-trip**: Insert JSON objects/arrays, read back, verify structure
- **JSON functions**: `JSON_EXTRACT`, `JSON_OBJECT`
- **Invalid JSON rejected**: Insert malformed JSON string
- **Persistence**: Close and reopen engine, verify JSON survives
- **NULL handling**: Nullable JSON column

## Acceptance Criteria

1. `CREATE TABLE t (id INT PRIMARY KEY, data JSON)` succeeds
2. JSON values round-trip through insert → select
3. `JSON_EXTRACT(data, '$.key')` works on JSON columns
4. Invalid JSON is rejected on insert
5. JSON data persists through engine close/reopen
