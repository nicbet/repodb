# Walkthrough: binary row encoding (format v3)

## What was built and why

Rows used to be stored as JSON arrays of `{null, value}` cells, where each value was base64-encoded text (`"42"`, an RFC 3339 timestamp, decimal text…). Decoding one cell meant a JSON unmarshal, a base64 decode, text parsing and then go-mysql-server's `Type.Convert`. After rdb-586f81 made scans stream, that decode was still about a third of full-scan time. Rows are now a compact binary blob, decoded straight into the Go values each column type uses. Full scans and wide ranges got about twice as fast (50k-row full scan: 66 → 32 ms), with about 40% fewer allocations.

This is a storage format bump (v2 → v3) with no migration: RepoDB is alpha.

## How the pieces fit together

- **`engine/rowcodec.go`** holds the whole codec. `encodeRow`/`decodeRow` keep their old signatures, so the catalog, range iterators and merge code didn't change. The old JSON codec (`cellDisk`, `rawValue`) is deleted from `catalog.go`.
- **Layout:** a uvarint column count, a NULL bitmap (`ceil(n/8)` bytes), then one cell per non-NULL column, in schema order. Rows carry no type tags: the schema already describes the cells, and the manifest carries the format version.
- **Cells:**
  - zigzag varints for signed integers and TIME;
  - uvarints for unsigned integers and ENUM indexes;
  - raw little-endian IEEE bits for FLOAT (4 bytes) and DOUBLE (8 bytes);
  - fixed 8-byte little-endian microseconds for DATE, DATETIME and TIMESTAMP;
  - a uvarint length plus bytes for strings, binary values and JSON text;
  - for DECIMAL, the unscaled integer at the column scale (see below).
- **Decoding** is done by `rowDecoder`, which walks the buffer by position. Every read is bounds-checked. Truncated input, trailing bytes, a wrong column count and unknown DECIMAL tags are errors, never panics, and `FuzzDecodeRow` enforces that.
- **Format versions:** `repository.FormatVersion` and `workingFormatVersion` both went to 3. The journal's typed edits embed row blobs, so journal records written in v2 are incompatible too. Opening a v2 repository fails with the existing "unsupported RepoDB format … re-initialize" error.

## Key decisions

- **Canonical bytes are a hard requirement.** Three-way merge (`sameValue` in `engine/merge.go`) compares row blobs byte for byte, so two equal rows must produce identical bytes, or merges report false conflicts. Each encoding is therefore a function of the value:
  - datetimes become microseconds, so the time zone doesn't matter;
  - DECIMALs are normalized to the column scale, and negative zero becomes zero.
  - Tests check that re-encoding a decoded row gives the same bytes, and they check specific equal-value pairs.
- **No `Convert` on decode (except JSON).** Values are converted to the column type when they are written, so the decoder can build the exact Go type directly: `int8`/`int16`/`int32` (INT24 is `int32`)/`int64`, the unsigned equivalents, `float32`/`float64`, `uint16` for ENUM, `time.Time` in UTC, `types.Timespan` and `*apd.Decimal`. The round-trip test asserts that the decoded type matches `Convert`'s output, and that running `Convert` on a decoded value changes nothing. JSON still goes through `Convert`, because the document has to be parsed. A binary JSON format would be separate work.
- **Integers use varints; datetimes are fixed-width** (user decision). Row blobs become git objects, so size matters: a BIGINT holding a small value takes 1–3 bytes instead of 8. Datetime microseconds are about 2⁵¹, so a varint would take 8–9 bytes for nothing. Keys stay fixed-width (`keycodec.go`) because keys need byte order and rows don't. The key codec can't be reused for values anyway: it is one-way (collation weights, escaping).
- **DECIMAL is native now** (user decision). go-mysql-server's column `Convert` rounds every value to exponent `-scale`, so "value × 10^scale" is an exact integer with only one encoding. It is stored as tag `0` plus a zigzag varint when it fits in int64 (precision ≤ 18 always does), or tag `1` plus a sign byte and a length-prefixed big-endian magnitude (up to precision 65). The encoder calls `sql.DecimalRound` whenever a value arrives with a different exponent, so the encoding stays canonical even for values that never went through `Convert`. The decoder builds `apd.Decimal{Exponent: -scale}` directly.
- **Changing the scale is safe because `ModifyColumn` rewrites rows.** A stored DECIMAL only means something together with its column's scale. On a type change, `ModifyColumn` converts every value and re-encodes the table. `TestAlterTableModifyDecimalScaleRoundTrips` checks this across an engine reopen: if rows weren't rewritten, 1234.56 would read back as 12.3456.

## Non-obvious details

- **One string copy per row.** The first string cell makes a single `string(data)` copy, and every string cell is a substring of it. That turns N string allocations into one. The catch is that one retained string keeps the whole row's copy alive. That's fine for row-sized data.
- **BLOB cells are copied individually** with `make` + `copy`. Callers may keep or modify `[]byte` values. `make` also keeps an empty BLOB non-nil (`[]byte{}`), where the old codec produced `[]byte(nil)`.
- **Negative DECIMAL magnitudes use `SetUint64(uint64(-v))`.** That also handles `MinInt64`, which fuzzed input can produce even though the encoder never writes it.
- **Test comparisons:** `sameCell` in `rowcodec_test.go` compares DECIMALs by value and exponent (apd's internal big-integer layout and the sign of zero can differ between equal values), floats by bits (so −0 ≠ 0), and JSON by canonical text.

## Measurements

`BenchmarkSQLAutocommitScans` (50k rows, journal, M1, median of 3) gained a `range10k` case, and `BenchmarkRowCodec` is new:

| case | before | after |
|---|---|---|
| fullscan | 66.4 ms, 857.5k allocs | 31.8 ms, 507.9k allocs |
| range10k | 13.3 ms | 6.34 ms |
| range100 | 0.242 ms | 0.137 ms |
| limit20 | 0.130 ms | 0.073 ms |
| row decode (8 mixed columns) | 2149 ns, 32 allocs, ~200 B | 197 ns, 9 allocs, 55 B |
| row encode | 1616 ns, 30 allocs | 84 ns, 1 alloc |

README and `latest.md` are unchanged; new numbers belong to the next scorecard refresh. What's left in a full scan is mostly the Prolly tree walk, go-mysql-server's row handling, and boxing values into `interface{}`.
