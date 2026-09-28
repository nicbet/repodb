# Order-preserving key format (v2) with range and ORDER BY/LIMIT pushdown

## What

Replace RepoDB's key encoding with one whose byte order equals SQL order. With that in place:
- implement non-point range lookups on the primary key and on secondary indexes;
- let go-mysql-server replace `ORDER BY <index columns> … LIMIT n` sorts with an ordered, streaming index scan.

This is a storage format change (v1 → v2) with no migration, per the user: RepoDB is alpha and no real repositories depend on it.

## Why

In the rdb-6ae82d scorecard, range (100 rows), ordered LIMIT and join cost 87–96 ms at 50k rows (17–19 ms at 10k), against 0.3–36 ms for MySQL and Dolt. Profiling an autocommit range/LIMIT/full scan at 50k rows showed ~60% of the time in `ensureRows`: decoding every row from JSON plus walking the whole tree on every query. The rest is the query engine filtering or sorting all 50k rows in memory.

Lookups were point-only because keys didn't sort in SQL order. `encodeKey` wrote `[type:2][len:4 BE][text]` with integers as decimal text, so keys sorted by length first, then bytes: `-10` > `-9` and `"b"` < `"aa"`. The Prolly tree therefore couldn't be seeked for a range.

## How

### 1. Key codec (`engine/keycodec.go`, new)

An injective, order-preserving encoding per column, concatenated for composite keys. Each column is self-delimiting, so a prefix orders correctly.

| SQL type | Encoding |
| --- | --- |
| signed ints (all widths) | 8-byte big-endian with the sign bit flipped |
| unsigned ints | 8-byte big-endian |
| ENUM | 2-byte big-endian index (MySQL orders ENUM by index) |
| FLOAT/DOUBLE | IEEE-754 bits: flip all bits if negative, else flip the sign bit. −0 is normalized to +0. NaN is rejected. |
| DECIMAL | Sign class byte (neg / zero / pos). For non-zero values, a sign-flipped 4-byte exponent, then digit bytes (digit+1) ending in `0x00`, with trailing zeros trimmed so `1.0` = `1.00`. Bitwise inverted after the class byte for negatives. |
| DATE/DATETIME/TIMESTAMP | int64 microseconds since the Unix epoch (UTC), sign-flipped big-endian. Pre-1970 dates order correctly. |
| TIME | int64 microseconds, sign-flipped big-endian |
| CHAR/VARCHAR/TEXT | Default binary collation: UTF-8 bytes. Other collations: 4-byte sign-flipped big-endian `Sorter()` weight per rune. CHAR drops trailing spaces first. Escaped `0x00 → 0x00 0xFF` and terminated `0x00 0x01`. |
| BINARY/VARBINARY/BLOB | Raw bytes, same escaping and terminator |

JSON and other non-key types are rejected.

- **Primary key** = concatenation of the PK columns' encodings (PK columns are non-NULL).
- **Secondary index key** = for each indexed column, `0x00` if NULL, else `0x01` + the encoding (NULL sorts first, as in MySQL), then the PK suffix, with the existing unique/non-unique rules.

`encodeKey` and `encodeIndexKey` use the codec. Keys are never decoded: row values stay in the row blob.

### 2. Format version

- `repository.FormatVersion` goes 1 → 2, and `workingFormatVersion` 1 → 2, because journal typed edits carry keys.
- Opening a v1 repository fails with "unsupported RepoDB format 1 (supported: 2); RepoDB is alpha and does not migrate data between formats, so re-initialize the data history". No migration.

### 3. Ranges on the primary key

- `primaryIndex.CanSupport` accepts `MySQLRange`s shaped as point columns, then at most one ranged column, then only unconstrained columns. Other shapes return false, and go-mysql-server falls back to a scan plus filter. Results are always exact, so `PreciseMatch` stays true.
- `LookupPartitions` maps ranges to sorted, merged `[start, end)` key intervals (`rangeIntervals`, `engine/ranges.go`). For example, a lower bound `Below(v)` gives `enc(v)`, and an upper bound `Above(v)` gives `keyPrefixEnd(enc(v))`.
- Full-PK non-NULL point ranges keep the point fast path. Their keys are now sorted and deduplicated, so `IN` lists return rows in index order.

### 4. Streaming, ordered scans

- `primaryRowIter` merges a `prolly.IteratorFrom(start)` over the base data tree with the pending overlay edits inside each interval: keys sorted per interval, overlay wins, deletes suppress base entries.
- It decodes rows lazily, verifying each row's key, and stops at the interval end, so a LIMIT above it stops early.
- When a transaction has materialized all rows (`state.rows`), it serves them in sorted key order.
- Unindexed full scans use the same iterator over the whole key space, instead of building every row with `ensureRows`. `ensureRows` remains for write and DDL paths.

### 5. Ranges on secondary indexes

`secondaryIndex.CanSupport` gets the same shape rule over the index columns. `indexRowIter` walks the index tree plus `idxEdits` (last edit per key wins) over the key interval, where the NULL marker handles `BelowNull`/`AboveNull`. It yields PKs in index order and fetches rows with `lookupRow`. All secondary lookups (unique and non-unique, points included) use it.

### 6. Ordered index for sort replacement

- `primaryIndex` and `secondaryIndex` implement `sql.OrderedIndex`: `Order` = `IndexOrderAsc`, and `Reversible` = false, since the Prolly iterator is forward-only.
- go-mysql-server drops `ORDER BY pk [ASC] LIMIT n` and `ORDER BY idxcol …` sorts. `ORDER BY … DESC` still sorts (follow-up rdb-acd36d).

### 7. Compatibility surface

- Merge conflict IDs and error messages embed base64 keys. Their bytes change, which is fine within the format bump.
- Tests that hard-coded format 1 expectations are updated.
- Data commit subjects follow the format version (`RepoDB snapshot v2`); storage-format.md says so instead of naming v1.

## Implementation notes (added during implementation)

- **Collated strings use per-rune `Sorter()` weights, not `WriteWeightString`.** The weight string is written little-endian (meant for hashing and equality), so it is not byte-orderable. The codec mirrors go-mysql-server's `StringType.Compare`: CHAR trims trailing spaces, then strings compare rune weight by rune weight, and a prefix sorts first.
- **Dead code removed.** The old secondary lookup helpers (`lookupUniquePartitions`, `lookupSecondaryPartitions`, `resolveIndexToPK(s)`) are gone. Routing unique points through the iterator also fixes `unique_col IS NULL`, which the old exact-key path could not answer. It was unreachable only because `CanSupport` rejected NULL ranges.
- **Legacy layout support is removed (user decision in review: "this is an alpha, there is no legacy").** This covers the `repodb import-legacy` command, `repository.ImportLegacy` (`legacy.go`), the tracked-`.repodb` detection in `Init`/`Open`, `ErrLegacyLayout`, the `Repository.Dir` field (used only by the importer), their tests and their docs. The importer could only ever have produced corrupt format 2 data from format 1 objects.
- **Interval ends are not symmetric (review fix).** A `nil` end means "no upper bound", but there is no key above an all-`0xFF` key. So an exclusive lower bound after the largest key (`k > MAX_BIGINT`, `k > MAX_UNSIGNED`, or `AboveAll` as a lower bound) is an **empty range**, not a `nil` start. `lowerCutKey` reports this explicitly and `rangeIntervals` drops the interval. `TestRangeQueriesAtIntegerExtremes` covers signed and unsigned extremes on primary keys and secondary indexes.
- **README performance prose is left as is.** It quotes latest.md (`57a0cd8`). New range numbers belong to the next scorecard refresh, per the publishing process.

## Decisions

- **No migration.** User decision: alpha, no real repositories. Old repositories are refused loudly, never misread.
- **Row encoding (JSON) unchanged.** Decoding now applies only to rows actually returned. A faster row codec is follow-up rdb-ee17d5.
- **Reverse scans later** (rdb-acd36d).
- **Composite ranges limited to "point prefix + one range column".** Anything else falls back to scan plus filter, which is still exact.

## Acceptance criteria (met)

- **Codec property tests** (`engine/keycodec_test.go`): int64, int8, uint64, float64, float32, decimal, datetime, time, enum, binary-collation varchar, case-insensitive varchar, CHAR and varbinary. Random and edge values; order and equality hold. Composite-key order and rejection of NaN/JSON/mistyped values are also covered.
- **Range correctness** (`TestRangeQueriesMatchFullScan`): signed, decimal, ci-string and datetime keys with a nullable secondary index. Each case runs in native-git, in journal with pending edits over checkpointed trees, and in an open transaction with uncommitted writes. `>`, `>=`, `<`, `<=`, `BETWEEN`, `IN`, index ranges and equality, `IS [NOT] NULL` and ordered LIMITs all match a reference query that is plainly a table scan plus sort. The test asserts the indexed side uses `IndexedTableAccess` and the reference side does not.
- **Extremes** (`TestRangeQueriesAtIntegerExtremes`): `>`, `>=`, `<`, `<=` and `BETWEEN` at MIN/MAX of BIGINT and BIGINT UNSIGNED, on the primary key and a secondary index.
- **Plans** (`TestRangeAndOrderPlansUseIndexes`): `IndexedTableAccess` on the expected index, and no Sort/TopN for ordered LIMITs.
- **Format:** a repository written by the previous release is refused with the new message (end-to-end with binaries). The unsupported-format and corrupt-snapshot checks stay tested; all legacy-layout code, tests and docs are removed.
- **Performance** (`BenchmarkSQLAutocommitScans`, 50k rows, journal): range100 109 → 0.25 ms; `ORDER BY id LIMIT 20` 96 → 0.14 ms; `id > 49000 ORDER BY id LIMIT 20` 0.12 ms; full scan 88 → 67 ms.
- `make test`, race tests and `make lint` pass. Follow-ups rdb-ee17d5 and rdb-acd36d are filed.
