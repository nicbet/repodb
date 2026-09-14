# Spec: Fuzz testing for SQL bind and storage paths

## What

Add Go native fuzz tests (`testing.F`) for the high-risk input paths in RepoDB: SQL parameter binding, row encoding/decoding, schema encoding/decoding, and primary key encoding.

## Why

The backslash escaping bug (rdb-1ed603) showed that subtle encoding issues slip past unit tests. Fuzz testing systematically explores edge cases in string escaping, type coercion, and serialization round-trips that hand-written tests miss. These paths are on the critical data integrity boundary — a bug here corrupts stored data.

## Scope

Four fuzz test files, three fuzz targets each where appropriate.

### 1. SQL bind path (`engine/fuzz_test.go`, package `engine`)

**Target: `FuzzSqlLiteral`**
- Fuzz `sqlLiteral()` with arbitrary strings
- Property: the output must be a valid SQL string literal that, when parsed by go-mysql-server, yields the original input
- Seed corpus: backslashes, single quotes, double quotes, null bytes, newlines, unicode, JSON payloads, empty string
- Round-trip: `bind("SELECT ?", []any{input})` → parse → extract value → compare to input

**Target: `FuzzBind`**
- Fuzz `bind()` with arbitrary statement + args combinations
- Property: `?` count must match args count, output must not panic, quoted regions must be respected
- Seed: statements with `?` inside single-quoted strings, escaped quotes, multiple parameters

### 2. Row encoding round-trip (`engine/fuzz_test.go`, package `engine`)

**Target: `FuzzRowRoundTrip`**
- Fuzz `encodeRow()` → `decodeRow()` round-trip
- For each supported type (INT64, FLOAT64, VARCHAR, BLOB, UINT64), construct a row from fuzzed bytes, encode, decode, compare
- Property: `decodeRow(encodeRow(row)) == row` for all valid inputs
- Seed: empty strings, large ints, float edge cases (NaN, Inf — verify they either round-trip or produce a clean error), binary data with null bytes

### 3. Schema encoding round-trip (`engine/fuzz_test.go`, package `engine`)

**Target: `FuzzSchemaRoundTrip`**
- Fuzz `encodeSchema()` → `decodeSchema()` with varying column names, types, nullability, defaults, checks
- Property: decoded schema matches encoded schema (column names, types, PK ordinals, check definitions)
- Seed: schemas with defaults, checks, various type combinations

### 4. Key encoding (`engine/fuzz_test.go`, package `engine`)

**Target: `FuzzEncodeKey`**
- Fuzz `encodeKey()` with arbitrary row values for supported PK types
- Property: must not panic; two distinct rows must produce distinct keys; same row must produce the same key
- Seed: empty strings, max/min int values, binary data

## Design

All fuzz tests go in `engine/fuzz_test.go` using `package engine` since the target functions are unexported. Each test uses `f.Add()` for seed corpus entries and `f.Fuzz()` for the property check.

For the SQL literal round-trip, the test will:
1. Call `sqlLiteral(input)` to get the SQL string
2. Wrap it in `SELECT <literal>` and execute via go-mysql-server's parser
3. Verify the parsed value matches the input

For row/schema round-trips, the test will encode then decode and compare.

## Acceptance criteria

- [ ] `FuzzSqlLiteral` catches strings that don't round-trip through SQL parsing
- [ ] `FuzzBind` verifies no panics on arbitrary statement/arg combinations
- [ ] `FuzzRowRoundTrip` verifies encode/decode identity for all supported types
- [ ] `FuzzSchemaRoundTrip` verifies schema encode/decode identity including defaults and checks
- [ ] `FuzzEncodeKey` verifies deterministic key encoding with no panics
- [ ] All fuzz tests pass with default `go test -fuzz` duration
- [ ] All existing tests continue to pass
- [ ] Seed corpus covers known edge cases (backslashes, quotes, null bytes, empty strings, unicode)