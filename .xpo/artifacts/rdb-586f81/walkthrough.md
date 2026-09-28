# Walkthrough: order-preserving keys (format v2), range and ORDER BY/LIMIT pushdown

## Why this was needed

Range, ordered-LIMIT and join queries took 87–96 ms at 50k rows. Every autocommit query rebuilt the whole table in memory: it walked the whole Prolly tree, decoded every JSON row, then let go-mysql-server filter or sort them.

The deeper reason was the key format. Format 1 keys were `[type][length][decimal text]`, which sort by length first. So `-10` sorted after `-9` and `"b"` before `"aa"`, the tree could not be seeked for a range, and indexes were point-only.

## The key codec (`engine/keycodec.go`)

Each column value becomes a self-delimiting byte string whose `bytes.Compare` order equals SQL order. Composite keys concatenate the columns.
- **Integers:** 8 bytes big-endian (signed ones with the sign bit flipped).
- **Floats:** IEEE bits, flipped for negatives; −0 is normalized.
- **DECIMAL:** a sign class byte, then an exponent and the normalized digits, inverted for negatives. `1.0` and `1.00` encode identically, as they must for unique keys.
- **Dates and times:** microseconds, sign-flipped.
- **ENUM:** its index.
- **Strings and binary:** bytes with `0x00` escaped and a `0x00 0x01` terminator, so a prefix sorts first.

Collated strings were the subtle case. go-mysql-server's `WriteWeightString`, which format 1 used for equality, writes weights little-endian: fine for hashing, wrong for ordering. The codec instead mirrors `StringType.Compare`: CHAR drops trailing spaces, then each rune's `Sorter()` weight is written as 4 sign-flipped big-endian bytes.

Secondary-index keys add a `0x00`/`0x01` NULL marker per column (NULL sorts first) and the PK suffix, as before. Keys are never decoded, so no decoder exists.

Property tests (`engine/keycodec_test.go`) check, for 13 types with random and edge values, that byte order equals `Type.Compare` and that equal values encode identically.

## Format v2, no migration

- `FormatVersion` and the journal's `workingFormatVersion` are both 2. The journal changes too, because typed edits carry keys.
- A format 1 repository is refused with an explicit "re-initialize" message (user decision: alpha, no real repositories).
- For the same reason, all legacy-layout support is removed: `import-legacy`, `ImportLegacy`, legacy detection, `ErrLegacyLayout`, `Repository.Dir`, their tests and their docs. Importing format 1 objects into v2 could only have produced corrupt data.

## Ranges (`engine/ranges.go`)

- **Shape rule.** `CanSupport` accepts ranges of the form "point columns, then at most one ranged column, then unconstrained columns". Anything else falls back to scan plus filter, so results are always exact and `PreciseMatch` stays true.
- **Intervals.** `rangeIntervals` turns go-mysql-server's cuts into `[start, end)` key intervals, sorted and merged. `Below(v)` is `enc(v)`, `Above(v)` is `keyPrefixEnd(enc(v))`, and the NULL markers position `BelowNull`/`AboveNull`. go-mysql-server has already converted bound values to the column type (flooring fractional integer bounds, handling overflow).
- **Asymmetric ends (review fix).** A `nil` end means "no upper limit", but a `nil` start means "from the first key". An exclusive lower bound after an all-`0xFF` key (`k > MAX_BIGINT`) has no start at all, so `lowerCutKey` reports the range as empty instead of returning `nil`. `TestRangeQueriesAtIntegerExtremes` covers this.

## Streaming scans

- **Primary key.** `primaryRowIter` merges a `prolly.IteratorFrom(start)` with the pending overlay (`state.edits`) per interval: overlay keys sorted, overlay wins, deletes suppress base rows. Rows are decoded only when returned, so a LIMIT stops the scan early. If the transaction has materialized all rows (after writes), it serves `state.rows` in sorted key order instead.
- **Full scans.** They use the same iterator over the whole key space instead of `ensureRows`, which remains for writes and DDL.
- **Secondary indexes.** `indexRowIter` does the same over the index tree plus `idxEdits` (last edit per key wins), yielding PKs in index order and fetching rows with `lookupRow`. Every secondary lookup uses it. The old exact-key helpers were removed, which also fixed `unique_col IS NULL`.
- **Point lookups.** Full-PK points keep their fast path, now with sorted keys so `IN` lists come back in index order.

## Sort elimination

Both index types implement `sql.OrderedIndex` (`Order` ascending, `Reversible` false). go-mysql-server's sort replacement then drops `ORDER BY pk LIMIT n` and `ORDER BY idxcol …` sorts, which `EXPLAIN PLAN` confirms (`TestRangeAndOrderPlansUseIndexes`). DESC still sorts until reverse iteration exists (rdb-acd36d).

## Verification

- **`TestRangeQueriesMatchFullScan`.** Signed, decimal, case-insensitive string and datetime keys with a nullable secondary index, each in native-git, in journal with pending edits over checkpointed trees, and in an open transaction with uncommitted writes. Every range, `IN`, `IS [NOT] NULL` and ordered LIMIT is compared with a reference that is plainly a table scan plus sort. The reference wraps columns in type- and collation-preserving no-ops, and the test asserts via EXPLAIN that the two sides take different paths.
- **End to end with binaries:** a format 1 repository is refused, and server range queries work with negative keys and a DECIMAL index containing NULLs.
- **Benchmark** (`BenchmarkSQLAutocommitScans`, 50k rows):
  - 100-row range: 109 → 0.25 ms;
  - `ORDER BY id LIMIT 20`: 96 → 0.14 ms;
  - `id > 49000 ORDER BY id LIMIT 20`: 0.12 ms;
  - full scan: 88 → 67 ms.

  The rest of the full-scan cost is JSON row decoding (rdb-ee17d5).
