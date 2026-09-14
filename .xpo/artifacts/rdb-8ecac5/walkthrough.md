# Walkthrough: Fuzz testing for SQL bind and storage paths

## What was built

11 Go native fuzz tests (`testing.F`) targeting RepoDB's high-risk input paths: SQL parameter binding, row encoding/decoding, schema encoding/decoding, and primary key encoding. Documentation added to `docs/fuzz-testing.md`.

## How the pieces fit together

All fuzz tests live in `engine/fuzz_test.go` under `package engine` because every target function (`sqlLiteral`, `bind`, `encodeRow`, `decodeRow`, `encodeSchema`, `decodeSchema`, `encodeKey`) is unexported.

Each test follows the same pattern:
1. `f.Add(...)` seeds with known edge cases
2. `f.Fuzz(func(t, input) { ... })` checks a property for every generated input

The properties tested are:
- **No panics** — `bind()` handles any statement/arg combination without crashing
- **Well-formedness** — `sqlLiteral()` produces valid SQL literals
- **Round-trip identity** — `decode(encode(x)) == x` for rows and schemas
- **Determinism** — same input produces the same key
- **Distinctness** — different PK values produce different keys

## Key decisions

**One file, not three.** All targets are in `engine/fuzz_test.go` rather than split across packages. The prolly tree doesn't have its own key encoding — that's done by `encodeKey` in `engine/catalog.go` — so all targets naturally belong in the engine package.

**Separate fuzz targets per type.** Row round-trips are split by type (int64, uint64, float64, string, blob) rather than using a single combined target. Go's fuzzer works best with simple input types, and splitting lets each target explore its type's edge cases independently.

**Float NaN/Inf skipped.** `FuzzRowRoundTripFloat64` skips NaN and Inf — these don't have a stable string representation through `fmt.Sprint` / `strconv.ParseFloat`, so they're expected to not round-trip. This is documented behavior, not a bug.

## Verification

Over 2.4 million executions across the high-risk targets with zero failures. See `docs/fuzz-testing.md` for the full results table and instructions for running longer fuzz sessions.