# Speed up full scans and joins

## What

Make whole-table reads cheap. Prolly nodes become a binary format that is read in place (storage format 4 → 5). Scans decode only the columns a query uses. The per-row key re-check leaves the scan path. Tables report row counts, so `COUNT(*)` needs no scan.

## Why

At 50k rows a full scan takes 32 ms and the scorecard join 35 ms in-process (34 and 45 ms in Docker, against MySQL's 13 and 2.6 ms). About two thirds of a scan is JSON/base64 node decoding and copying. A prototype of the changes below measured a full scan at 12.7 ms, the join at 12.8 ms and `COUNT(*)` at 4.3 ms (before row counts).

## Acceptance Criteria

- In-process benchmarks at 50k rows, journal, checkpointed: `BenchmarkSQLAutocommitScans/fullscan` ≤ 13 ms, the scorecard join ≤ 15 ms, `SELECT COUNT(*) FROM bench` ≤ 1 ms. A new `join` and `count` case is added to `BenchmarkSQLAutocommitScans`.
- `make bench-docker BENCH_MODE=journal` before (latest.md at `636af3d`) and after. Results and the per-scan CPU profile are recorded in the issue.
- Format 4 repositories are refused with the existing unsupported-format error.
- Every existing test passes. New tests cover the node codec (round trip, truncated or trailing bytes, bad header) and projections (subset, reorder, zero columns, through ranges, secondary indexes, point lookups and pending edits). They also cover exact versus estimated `RowCount`.
- `docs/architecture.md` describes the new node format, counts, the store contract and format version 5. It also fixes the stale "currently 3". `docs/sql.md` gets an update if projection or `COUNT(*)` behavior is user-visible.

## Flow

1. **Node codec (`common/prolly/codec.go`, used by `tree.go`)**
   - Leaf: `'P'`, codec version `1`, level `0`, uvarint count, then per entry uvarint key length, key, uvarint value length, value.
   - Interior: the same header with level ≥ 1, then per child uvarint max-key length, max key, uvarint subtree row count, 32 raw hash bytes.
   - Decoding checks bounds, count and trailing bytes, and returns slices that alias the stored bytes (capacity clipped).
   - `link` gains `Count uint64`; `Build`, `Apply`, `SortedBuilder` and `buildFromLeafLinks` fill it. `Tree.Count(ctx)` sums the root's links, or returns the leaf's entry count.
2. **Store contract (`common/storage`, `common/repository`)**: `Store.Get` returns bytes the caller must not modify, and they may be shared. `snapshotStore.Get`, `Writer.Get` and `Memory.Get` stop copying. Audit every `Get` and every Prolly `Entry` consumer for in-place modification. `Iterator.Next` documents read-only bytes that stay valid indefinitely. `Hash.Valid` stops allocating.
3. **In-place leaf iteration**: the iterator keeps the current leaf's bytes and offset, and `Next` parses one entry, with no per-node entry slice. `Get` and `seekTo` scan a leaf in place: at most 128 entries, so a linear scan with an early exit. `Get` also walks interior nodes in place, converting only the chosen child's hash, and still returns a copy of the value.
4. **Format bump**: `repository.FormatVersion = 5`, `snapshotValidationVersion` bumped.
5. **Per-row scan work (`engine/ranges.go`)**: remove `encodeKey` from `primaryRowIter.Next`, since `ValidateSnapshot` decodes every stored row and checks its key once per snapshot. `mergedEntries` converts tree keys to strings only when an overlay is present.
6. **Projection pushdown (`engine/catalog.go`)**
   - `table` implements `sql.ProjectedTable`. `WithProjections` returns a copy with column ordinals, and `Schema` returns the projected columns.
   - Every row iterator (full and range scans, secondary-index iterator, point partitions, materialized `rows` and overlay rows) produces projected rows.
   - `decodeRowProjected` skips cells that aren't requested; zero columns skips decoding.
   - An identity projection is treated as none. A column projected twice is filled from its first decoded copy.
   - Editors (insert/update/delete) and DDL always work on the full schema.
7. **Row counts**: `table` implements `sql.StatisticsTable`.
   - `RowCount` is exact (the tree's count) when the transaction has no pending or local edits, and exact from `rows` when they are materialized.
   - Otherwise it returns an estimate (tree count plus the number of local edits), `exact = false`. It does not inspect the edits, because counting non-deletes would cost O(edits) on every query plan.
   - `DataLength` returns 0.
8. **Benchmarks and docs**: add `join` and `count` to `BenchmarkSQLAutocommitScans`, then the docs updates above.

## Decisions

- **Binary nodes read in place, not a decoded-node cache.** Reading in place costs about 0.4 ms per 50k-row table and needs no memory or eviction policy. A cache would hold a second copy of every table.
- **Length-prefixed entries, not an offset table.** Nodes hold ≤ 128 entries, so a linear in-place search is cheap. An offset table would add 8 bytes per entry for nothing measurable.
- **Subtree counts in links now.** They're cheap during this format bump and need another bump later. Boundaries still hash only keys, so counts don't affect chunking.
- **Key re-check moves out of the scan, not into a debug flag.** `ValidateSnapshot` already performs it for every snapshot the engine opens, and local commits only write keys produced by `encodeKey`.
- **Joins: no go-mysql-server changes.** Constant-table elimination and its executor's per-row group-key hashing stay as they are. Projection pushdown alone puts the join at about Dolt's time.
- **Chunk-size tuning (rdb-a0d510) stays separate.**

## Edge Cases

- HIGH: In-place modification of shared store bytes would corrupt cached objects for every reader. Step 2's audit must cover `prolly` (`Apply`, the chunkers clone entries already), `engine` (merge, keys), `integration` and `repository`. Decoded keys and values have their capacity clipped, so an append copies (tested). Callers that modify bytes clone first. Audit result: no caller writes into returned bytes. Merge only compares and emits through `SortedBuilder.Add`, which clones. Binary row cells are copied by `decodeRow`. Schema bytes are only JSON-decoded or passed to `Writer.Put`, which copies.
- MEDIUM: go-mysql-server may call `WithProjections` on tables later used for indexed access or writes. Projection must carry through `IndexedAccess`, and editors must ignore it.
- MEDIUM: Planner choices may change once `RowCount` exists (go-mysql-server costs joins and index scans with it). The existing EXPLAIN tests guard this; review any plan changes.
- LOW: An empty tree is a level-0 node with count 0. `Count` handles it.

## Assumptions

- Projection pushdown helps the scorecard join and aggregates. `SELECT *` gains nothing from it.
- `RowCount` is called during planning for every query, so it must not scan: it reads the root node only.
- go-mysql-server caches prepared statements as parsed ASTs and re-analyzes each execution, so a `table_count` plan never carries a stale count.
- go-mysql-server pushes projections only below read-only nodes (Filter, Project, GroupBy, Sort, Limit, joins…). UPDATE and DELETE targets are never pruned, so editors always receive full rows.
