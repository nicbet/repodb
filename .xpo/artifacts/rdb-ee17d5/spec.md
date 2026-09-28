# Faster row encoding (format v3)

## What

Replace the JSON row blob (`[{"null":..,"value":"<base64 text>"}]`) with a compact binary row codec that is decoded without `encoding/json`, base64, text parsing or (for common types) `Type.Convert`. It is a storage format bump (v2 → v3) with no migration, like rdb-586f81.

## Why

Profiling autocommit scans at 50k rows put `decodeRow` at about 33% of query time, with heavy allocation. rdb-586f81 made scans decode only the rows they return, but full scans and wide ranges still decode every row (full scan: 67 ms at 50k rows in `BenchmarkSQLAutocommitScans`). Per cell, the current path does a JSON unmarshal, a base64 decode, text parsing (`strconv`, `time.Parse`), and `Type.Convert`.

## How

### 1. Row layout (`engine/rowcodec.go`, new)

```
uvarint   column count
bytes     NULL bitmap, ceil(n/8) bytes, bit i set = column i is NULL
cells     one per non-NULL column, in schema order
```

| SQL type | Cell encoding | Decoded Go value |
| --- | --- | --- |
| INT8…INT64 | zigzag varint | the column's width (`int8`…`int64`) |
| UINT8…UINT64 | uvarint | `uint8`…`uint64` |
| ENUM | uvarint index | `uint16` |
| FLOAT | 4-byte little-endian IEEE bits | `float32` |
| DOUBLE | 8-byte little-endian IEEE bits | `float64` |
| DATE/DATETIME/TIMESTAMP | 8-byte little-endian int64 microseconds since the Unix epoch | `time.Time` in UTC |
| TIME | zigzag varint (Timespan microseconds) | `types.Timespan` |
| CHAR/VARCHAR/TEXT | uvarint length + UTF-8 bytes | `string` |
| BINARY/VARBINARY/BLOB | uvarint length + bytes | `[]byte` |
| DECIMAL | tag byte, then the unscaled value (value × 10^scale): tag `0` = zigzag varint (fits int64), tag `1` = sign byte + uvarint length + big-endian magnitude | `*apd.Decimal` with exponent `-scale` |
| JSON | uvarint length + `JsonToMySqlString` text (as today) | via `types.JSON.Convert`, as today |

- **Canonical bytes.** Three-way merge compares row blobs byte for byte (`sameValue`, `engine/merge.go`), so equal rows must encode identically. Each encoding above is a function of the stored value. Floats keep their exact bits (today's text also keeps `-0` distinct from `0`).
- **Decoding without Convert.** Values were converted to the column type on write, so the decoder builds the Go type the column expects directly. Only JSON keeps text plus `Convert`: its canonical text is the value, and a binary JSON format is separate work.
- **DECIMAL.** go-mysql-server's column `Convert` rounds every value to the column scale (exponent = `-scale`), so the unscaled integer at that scale is exact and canonical. The encoder rescales defensively (a value with a different exponent is rounded to the column scale, the same as `Convert`) and normalizes negative zero to zero. The decoder builds the `apd.Decimal` directly, with no string parse, `Convert` or bounds check. The small form covers precision ≤ 18 (e.g. `DECIMAL(10,2)`); the big form covers up to precision 65.
- **Fewer allocations.** One `sql.Row` per row. String cells are substrings of a single `string(data)` copy made once per row (only when the schema has string columns). Binary cells stay per-cell copies, since callers may retain or mutate `[]byte`.
- **Strict decoding.** Truncated input, trailing bytes, a column count that doesn't match the schema, and invalid varints are errors, never panics. The existing row fuzz tests cover this.
- `encodeRow`/`decodeRow` keep their signatures, so callers (catalog, ranges, merge) are unchanged. `rawValue` stays for any non-row uses; if the row codec was its only caller, it is removed.

### 2. Format version

- `repository.FormatVersion` 2 → 3. The journal's typed edits carry row blobs, so `workingFormatVersion` also goes 2 → 3.
- Opening a v2 repository fails with the existing "unsupported RepoDB format 2 (supported: 3) … re-initialize" message. Snapshot subjects become `RepoDB snapshot v3`.
- `docs/storage-format.md` documents the row layout and says v3. Tests that hard-code v2 are updated.

### 3. Measurement

Run `BenchmarkSQLAutocommitScans` (50k rows, journal) before and after (`fullscan`, `range100`, plus a new `range10k` case: `id BETWEEN 20000 AND 29999`), with `-benchmem`. Add a micro-benchmark `BenchmarkRowCodec` (encode and decode of a mixed-type row). Record the numbers in the completion comment and walkthrough. README and latest.md are not changed; that waits for the next scorecard refresh (same as rdb-586f81).

## Decisions

- **No migration** (alpha; per the user's standing decision in rdb-586f81 and project memory).
- **Varints for integers, ENUM and TIME; fixed 8 bytes for datetimes** (user decision). Row blobs are git objects, so size matters: a BIGINT holding a small value takes 1–3 bytes instead of 8, and varint decoding costs a few ns against the hundreds per cell it replaces. Datetime microseconds are ~2⁵¹, so a varint would take 8–9 bytes and gain nothing. Keys stay fixed-width because they need byte order; rows don't.
- **Native DECIMAL now, not later** (user decision).
- **No reuse of the key codec for values.** Key encodings are order-preserving and one-way (collation weights, escaping). Rows must round-trip the exact value, so they need a different encoding.
- **No per-column schema tag or version byte in the row.** The format version lives in the manifest, and the schema describes the cells.

## Acceptance criteria

- Round-trip tests for every supported type, including NULLs, empty strings/blobs, extremes (MIN/MAX ints, `-0`, ±Inf, pre-1970 and year-9999 datetimes, negative TIME, DECIMAL zero/negative zero/max precision 65 and precision ≤ 18 boundary), multibyte UTF-8, and wide rows (>8 columns so the bitmap spans bytes). Decoded values equal what the previous codec produced after `Convert` (DECIMAL compared by value and exponent).
- Changing a DECIMAL column's scale (`ALTER TABLE … MODIFY`) keeps values correct.
- Existing row fuzz tests pass against the new codec. A new fuzz target feeds arbitrary bytes to `decodeRow` and must not panic.
- Determinism: the same row always encodes to the same bytes (merge relies on this).
- Opening a v2 repository is refused with the unsupported-format message.
- `fullscan` at 50k rows is measurably faster, with fewer allocs/op; before/after numbers are recorded.
- `make test`, race tests and `make lint` pass.
