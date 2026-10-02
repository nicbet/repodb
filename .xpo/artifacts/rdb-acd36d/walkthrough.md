# Walkthrough: reverse index scans for ORDER BY … DESC

## What was built and why

Indexes were `sql.OrderedIndex` with `Reversible = false`, because the Prolly iterator only went forward. go-mysql-server therefore kept a Sort or TopN for every `ORDER BY … DESC`, so `ORDER BY id DESC LIMIT 20` (the "latest N rows" query) read and sorted the whole table: 18 ms at 50k rows in Docker. Both indexes are now reversible, and every layer a scan merges can be walked backwards. That query now takes 39 µs, the same as ascending.

## How go-mysql-server asks for it

With `Reversible() = true`, `replaceIdxSort` rebuilds the lookup with `IsReverse` set and drops the Sort. A full-table `ORDER BY pk DESC` arrives as a reverse lookup over the full range. RepoDB only has to honor `lookup.IsReverse` in `LookupPartitions`. Merge joins with `IsReversed` use the same lookups.

## How the pieces fit

- **`prolly.ReverseIterator(ctx, end)`** (`common/prolly/reverse.go`) returns the entries below `end`, descending.
  - `seekBelow` descends into the first child whose max key reaches `end`; all earlier children lie entirely below it. Frames count down.
  - `retreat` and `descendRight` move to the previous leaf.
  - Leaves are decoded whole (`readNode`) and read backwards, because entries are length-prefixed only forward.
  - `EntryIterator` is the shared interface, so the forward iterator keeps its in-place fast path.
- **`PendingRows.IterReverse(start, end)`**: per run, the last index below `end`, then a k-way merge choosing the largest key, newest run winning, stopping below `start`.
- **Engine (`engine/ranges.go`):**
  - `before(a, b, reverse)` is the single place that knows the direction.
  - `sortedOverlayKeys` emits local keys in scan order; `pendingCursor` and `rowOverlayCursor` take a direction.
  - `mergedEntries` picks `ReverseIterator(iv.end)` or `IteratorFrom(iv.start)`, and bounds by the opposite end.
  - `scanOrder` visits the sorted, merged intervals last to first.
- **Partitions (`engine/catalog.go`):** `rangePartition.reverse` comes from `lookup.IsReverse`, and primary-key point lists are reversed.

## Key decisions

- **No format change for reverse reads.** Decoding a leaf whole costs one entry slice per leaf, negligible for LIMIT queries. Back-pointers would cost bytes in every node.
- **One `reverse` flag, not mirrored types.** The merge logic is identical apart from the comparison, so `before` keeps both directions in one code path.
- **Tie and NULL order:** a non-unique index key carries the PK suffix, so ties come in descending PK order. NULL index keys sort first, so a DESC scan returns them last, as MySQL does.

## Tests

- `TestReverseIteratorMatchesForward`: trees of 0 to 5,000 entries, with bounds of none, before all keys, between keys, on a key and past the end.
- `PendingRows`: the reverse walk equals the forward walk reversed in the random model, plus a reverse interval check.
- The range model runs every predicate in both directions, plus DESC ordered limits, including all-rows `ORDER BY idx DESC, pk DESC` with NULLs, over native-git, journal with pending edits and open transactions.
- The plan test asserts `reverse: true` and no Sort.
- A mutation check (ignoring `IsReverse` for primary ranges) fails the model test.
