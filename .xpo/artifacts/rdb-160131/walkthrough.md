# Support native JSON type

RepoDB now supports a native JSON column type. Previously, JSON data could only be stored as TEXT, which provided no validation and no access to MySQL JSON functions.

## How the pieces fit together

Three switch cases added to catalog.go, following the existing per-type pattern.

### Type mapping (`decodeType`)

`Type_JSON` → `types.JSON` — the singleton `JsonType{}` from go-mysql-server. No parameters (no length or precision). The type validates JSON on `Convert()` and wraps parsed values in `JSONDocument{Val: any}`.

### Storage encoding

**`rawValue()`**: JSON values arrive as `sql.JSONWrapper` (typically `types.JSONDocument`). Serialized via `types.JsonToMySqlString()`, which produces MySQL-canonical JSON (alphabetical keys, consistent spacing). This is a string representation stored as base64 in the cell, same as other text-like types.

**`decodeRow()`**: The stored bytes are a JSON string. Passed through `types.JSON.Convert()` which parses the string back into a `JSONDocument`. This ensures the round-tripped value supports all JSON operations (extract, compare, etc.).

### What didn't need to change

- **`sqlLiteral()`**: JSON values are passed as strings by API callers, which already works through the existing string case.
- **`encodeSchema()`/`decodeSchema()`**: JSON has no parameters (no length, precision, or enum values), so the existing `columnDisk` serializes it with just the type code.
- **`encodeKey()`**: JSON as a primary key is unusual but works — `rawValue()` produces deterministic canonical JSON strings.

### Code quality note

A comment was added on `decodeType()` noting that the per-type switch statements (`decodeType`, `rawValue`, `decodeRow`) could be collapsed into a type registry keyed by `querypb.Type`. This is a future cleanup opportunity — the current pattern works but requires touching three switches per new type.

### Tests

7 new tests, all asserting actual values (not just row counts):

- **DDL**: CREATE TABLE with JSON column, SHOW CREATE TABLE includes "json"
- **Round-trip**: Insert JSON object, array, and scalar string → read back → exact canonical JSON string comparison (`{"age": 30, "name": "alice"}`, `[1, 2, 3]`, `"just a string"`)
- **JSON_EXTRACT**: `JSON_UNQUOTE(JSON_EXTRACT(data, '$.name'))` returns `"alice"`
- **JSON_OBJECT**: Insert via `JSON_OBJECT('key', 'value')` → extract returns `"value"`
- **Invalid JSON**: `'{not valid json}'` is rejected on insert
- **Nullable**: NULL JSON column returns nil
- **Persistence**: Insert nested JSON, close/reopen engine, verify `$.key` = `"value"` and `$.nested.a` = `1`

## Key decisions

**`JsonToMySqlString` for serialization, not `json.Marshal`.** The MySQL-canonical format ensures the stored representation matches what go-mysql-server produces, avoiding any format divergence on round-trip. Keys are sorted alphabetically, spacing is consistent.

**No `sql.JSONWrapper` import in tests.** The external test package (`engine_test`) uses `fmt.Sprint()` for exact canonical JSON string comparison rather than importing internal types for `ToInterface()`. This keeps test dependencies minimal while still providing byte-exact assertions.
