# Fuzz Testing

RepoDB uses Go's native fuzz testing (`testing.F`) to stress-test high-risk
input paths. Fuzz tests live in `engine/fuzz_test.go` under `package engine`
(the target functions are unexported).

## Targets

### SQL bind path

| Target | Property |
|--------|----------|
| `FuzzSqlLiteral` | Arbitrary strings produce well-formed SQL literals via `sqlLiteral()` |
| `FuzzBindNoPanic` | `bind()` never panics on any statement/arg combination |

These cover the parameter interpolation layer that sits between user values and
the go-mysql-server parser. The backslash escaping bug (rdb-1ed603) was in this
path.

### Row encoding round-trips

| Target | Type |
|--------|------|
| `FuzzRowRoundTripInt64` | `INT64` |
| `FuzzRowRoundTripUint64` | `UINT64` |
| `FuzzRowRoundTripFloat64` | `FLOAT64` (skips NaN/Inf) |
| `FuzzRowRoundTripString` | `VARCHAR` |
| `FuzzRowRoundTripBlob` | `BLOB` |

Each verifies `decodeRow(encodeRow(row)) == row` for the given column type.

### Schema encoding round-trip

| Target | Property |
|--------|----------|
| `FuzzSchemaRoundTrip` | `decodeSchema(encodeSchema(s, c))` preserves column names, types, PK ordinals, and check definitions |

### Key encoding

| Target | Property |
|--------|----------|
| `FuzzEncodeKey` | Same int64 value produces the same key (determinism) |
| `FuzzEncodeKeyString` | Same string value produces the same key (determinism) |
| `FuzzEncodeKeyDistinct` | Distinct int64 values produce distinct keys |

## Running

Run all fuzz tests with seed corpus only (fast, part of `go test`):

```sh
go test ./engine/ -run Fuzz
```

Run a single target in fuzz mode for a duration:

```sh
go test ./engine/ -fuzz=FuzzSqlLiteral -fuzztime=60s
go test ./engine/ -fuzz=FuzzRowRoundTripString -fuzztime=60s
```

Run all targets sequentially for 30 seconds each:

```sh
for t in FuzzSqlLiteral FuzzBindNoPanic FuzzRowRoundTripInt64 \
         FuzzRowRoundTripUint64 FuzzRowRoundTripFloat64 \
         FuzzRowRoundTripString FuzzRowRoundTripBlob \
         FuzzSchemaRoundTrip FuzzEncodeKey FuzzEncodeKeyString \
         FuzzEncodeKeyDistinct; do
  echo "--- $t ---"
  go test ./engine/ -fuzz=$t -fuzztime=30s
done
```

## Initial results (2026-09-14)

| Target | Duration | Executions | Failures |
|--------|----------|------------|----------|
| `FuzzSqlLiteral` | 60s | 1,075,580 | 0 |
| `FuzzRowRoundTripString` | 60s | 501,352 | 0 |
| `FuzzBindNoPanic` | 30s | 438,013 | 0 |
| `FuzzEncodeKeyDistinct` | 30s | 401,066 | 0 |
| `FuzzRowRoundTripBlob` | 30s | 35,983 | 0 |

Over 2.4 million total executions across the high-risk targets with zero
failures found.

## Seed corpus

Each target includes hand-picked seeds covering known edge cases:

- Backslashes, single/double quotes, mixed escapes
- Null bytes, newlines, tabs
- Unicode and emoji
- JSON payloads (motivated by the rdb-1ed603 backslash bug)
- Empty strings
- Integer boundaries (MaxInt64, MinInt64, MaxUint64)
- Float edge cases (MaxFloat64, SmallestNonzeroFloat64)

The fuzzer extends these seeds automatically during `-fuzz` runs. Interesting
inputs discovered by the fuzzer are cached in `testdata/fuzz/` and replayed on
subsequent `go test` runs.
