# Walkthrough: faster full scans and joins (format 5)

## What was built and why

At 50k rows a full scan took 34 ms and the scorecard join 45 ms in Docker (MySQL 13 and 2.6 ms, Dolt 20 and 12 ms). Profiling showed:

- **Two thirds of a scan was node decoding.** Prolly nodes were JSON with base64 keys and values, and every scan JSON-decoded every node again. The snapshot cache also handed out a fresh copy of every node on each read.
- **The join was not a planning problem.** go-mysql-server already plans `ON a.id = 1` as an index lookup, so the plan is a CrossJoin in which one `authors` row drives one scan of `bench`. `bench` contributes no columns, yet every row was decoded.

The fix has four parts: a binary node format read in place, column projection, cheaper per-row work, and stored row counts. Storage format goes from 4 to 5, with no migration (alpha).

## How the pieces fit

### Node codec (`common/prolly/codec.go`)

```text
'P' | codec version (1) | level | uvarint count | items
leaf item:     uvarint key len, key, uvarint value len, value
interior item: uvarint max-key len, max key, uvarint subtree entry count, 32-byte SHA-256
```

`nodeReader` parses one item at a time. Keys and values are sub-slices of the stored bytes, with capacity clipped so an append copies instead of overwriting the next item. `decodeNode` builds a whole `node` (for `Apply`, `walk` and interior levels) and rejects truncation, trailing bytes, unknown headers and codec versions.

### Tree (`common/prolly/tree.go`)

- **The iterator keeps a `nodeReader` for its leaf.** `Next` parses one entry, so no per-node entry slice exists. Interior nodes (about 1 per 65 leaves) are still decoded whole.
- **`Get` walks in place** and converts only the chosen child's hash to hex. It still returns a copy of the value.
- **`link.Count`** is filled by `Build`, `Apply`, `SortedBuilder` and `buildFromLeafLinks` (`sumCounts`). `Tree.Count` reads only the root.
- **`walk` returns its subtree's count** and fails if a link's count disagrees, so `Reachable` and `ValidateSnapshot` verify counts. Its `seen` map now stores counts, so shared subtrees are still walked once.

### Store contract (`common/storage`)

`Store.Get` returns the stored bytes themselves, so callers must not modify them. `snapshotStore`, `Writer` and `Memory` no longer copy. The audit found no caller that writes into returned bytes:
- merge compares entries and emits through `SortedBuilder.Add`, which clones;
- `decodeRow` copies binary cells;
- schema bytes are only JSON-decoded or passed to `Put`, which copies.

`Hash.Valid` checks hex characters without allocating, upper- and lowercase as before.

### Projection (`engine/catalog.go`, `engine/rowcodec.go`)

- `table` implements `sql.ProjectedTable`. `WithProjections` returns a copy carrying a `rowProjection` (ordinals, a per-column output slot, the last needed column), or nil for the identity projection. `Schema` returns the projected columns.
- `decodeRowProjected` walks the NULL bitmap and cells, decoding projected cells and `skip`ping the others. It stops after the last needed column, and with no columns it doesn't touch the bytes.
- Every read path projects: full and range scans, secondary-index scans, point partitions, materialized rows and overlay rows.

go-mysql-server pushes projections only below read-only nodes, so writes always see full rows.

### Row counts

`table` implements `sql.StatisticsTable`. `RowCount` is exact in two cases:
- the tree count, when the transaction sees no pending or local edits (`hasEdits`);
- `len(rows)`, when rows are materialized.

Otherwise it returns an estimate (the tree count plus the number of edits) with `exact = false`, so go-mysql-server falls back to counting. When exact, `replaceCountStar` turns `SELECT COUNT(*) FROM t` into `table_count(t)`.

### Per-row work (`engine/ranges.go`)

- `primaryRowIter` no longer re-encodes every row's key to compare with the stored key. `ValidateSnapshot` does that once per snapshot.
- `mergedEntries` no longer converts tree keys to strings, since callers only use the key for overlay entries.

## Key decisions

- **Read in place, not a decoded-node cache.** In-place decoding of a 50k-row table costs about 0.4 ms. A cache would hold a second copy of every table.
- **Length-prefixed items, not an offset table.** Nodes hold ≤ 128 entries, so a linear in-place search is cheap.
- **Counts now.** They come free with this format bump, and boundaries still hash only keys.
- **`RowCount`'s estimate doesn't inspect the edits.** go-mysql-server calls it while planning every query, so it must be O(1).
- **No go-mysql-server changes for joins.** MySQL's 2.6 ms comes from constant-table elimination plus an index-only count. What remains after projection is go-mysql-server's executor: per-row group-key hashing and joined-row allocation.

## Results

In-process, M1 Max, 50k rows, journal:

| | before | after |
|---|---:|---:|
| fullscan | 32.7 ms | 12.3 ms |
| join | ~35 ms | 12.7 ms |
| `COUNT(*)` | ~28 ms | 27 µs |
| range100 | 143 µs | 62 µs |

`make bench-docker` (journal, diagnostic, 50k rows):

| | before | after | MySQL | Dolt |
|---|---:|---:|---:|---:|
| full scan | 34.1 ms | 13.4 ms | 12.9 ms | 20.5 ms |
| join | 45.0 ms | 14.6 ms | 2.6 ms | 11.8 ms |
| point read | 0.075 ms | 0.034 ms | 0.037 ms | 0.140 ms |

## Non-obvious

- **Use Linux for profiles.** On macOS, CPU profiles of this workload badly undercount: rusage shows about 15 ms of CPU per 12 ms scan across threads, while pprof sees about 4 ms.
- **Rebased onto rdb-a6a4d2.** `primaryRowIter` takes overlay edits from `rowOverlayCursor`, so `mergedEntries.next`'s key is ignored for tree rows. `RowCount` checks `hasEdits()`, so pending journal edits make the count an estimate; `TestCountStarUsesRowCount` covers this.
- **`latest.md` is not updated.** A scorecard is published from a clean commit of every mode.
