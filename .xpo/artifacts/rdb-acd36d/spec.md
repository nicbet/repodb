# Reverse index scans for ORDER BY … DESC

## What

Primary and secondary indexes become reversible (`sql.OrderedIndex.Reversible = true`). go-mysql-server then drops the Sort for `ORDER BY <index columns> DESC [LIMIT n]` and asks for a reverse lookup (`IndexLookup.IsReverse`). RepoDB serves it by walking the Prolly tree and every overlay layer right to left, so `ORDER BY id DESC LIMIT 20` reads about 20 rows instead of sorting the table.

## Why

At 50k rows the scorecard's `ordered_limit` (`ORDER BY id DESC LIMIT 20`) takes 18 ms in Docker. MySQL takes 0.06 ms and Dolt 0.21 ms, and the ascending form here takes about 0.04 ms. "Latest N rows" is a common application query.

## Acceptance Criteria

- `EXPLAIN` of `SELECT … ORDER BY id DESC LIMIT 20` and of `… WHERE b = ? ORDER BY b DESC` shows an `IndexedTableAccess` with `reverse: true`, and no Sort or TopN.
- At 50k rows, `ORDER BY id DESC LIMIT 20` is within 1.5× of `ORDER BY id LIMIT 20` (new `desc-limit20` case in `BenchmarkSQLAutocommitScans`).
- Results match a forced sort for:
  - primary and secondary indexes;
  - multi-range and `IN` lookups;
  - NULL index values;
  - native-git, journal with pending edits, and open transactions with their own edits.
  This extends the range model test in `range_test.go` with DESC orderings.
- `make test` and `make lint` pass. `docs/sql.md` and `architecture.md` drop the "DESC sorts" limitation.

## Flow

1. **Prolly (`common/prolly/tree.go`)**: `Tree.ReverseIterator(ctx, end []byte)` returns the entries with key < `end` (nil = all) in descending order.
   - It keeps a stack of interior frames whose `next` index walks down.
   - Leaves are decoded whole (`decodeNode`), because length-prefixed entries can only be parsed forward. That's one entry slice per leaf, read backwards.
   - It shares `Next`/`Close` with the forward iterator through an `EntryIterator` interface.
2. **`repository.PendingRows.IterReverse(start, end)`**: per run, binary-search the last key < `end`, then a k-way merge choosing the largest key, newest run winning, stopping below `start`.
3. **Engine overlays (`engine/ranges.go`)**: `keyCursor`, `rowOverlayCursor` and `pendingCursor` take a direction. Local keys are walked from the end, and pending iterators come from `IterReverse`. Each cursor's `peek` yields keys in the scan's order.
4. **`mergedEntries`** takes a direction. It compares overlay and tree keys with the order reversed, stops at `start` instead of `end`, and uses `ReverseIterator(iv.end)` instead of `IteratorFrom(iv.start)`.
5. **Partitions (`engine/catalog.go`)**:
   - `rangePartition` and `pointPartition` gain `reverse`, set from `lookup.IsReverse` in `LookupPartitions`.
   - Reverse range iterators walk their intervals last to first. Reverse point partitions walk their sorted keys backwards.
   - The materialized-rows path (`state.rows`) walks its sorted keys backwards.
6. **`primaryIndex.Reversible` and `secondaryIndex.Reversible`** return true.
7. **Tests and benchmark**: DESC cases in the range model, EXPLAIN assertions, a reverse unit test for Prolly and for `PendingRows`, and `desc-limit20` in `BenchmarkSQLAutocommitScans`.

## Decisions

- **Decode reverse leaves whole.** Adding back-pointers or an offset table to the node format for reverse parsing would cost bytes on every node for a rarer access path. A reverse scan allocates one entry slice per leaf (≤ 128 entries), which is negligible for a LIMIT query, and bounded per leaf for a full reverse scan.
- **One iterator interface, two implementations.** The forward iterator keeps its in-place fast path.
- **Ties within a non-unique index come in descending primary-key order.** Index keys carry the PK suffix. MySQL leaves the order of ties unspecified.

## Edge Cases

- MEDIUM: Multi-range lookups (`IN (…)`, OR'd ranges) must return ranges last to first. go-mysql-server drops the sort only for non-overlapping ranges (`hasOverlapping`), and they are sorted ascending.
- MEDIUM: NULL index keys sort first (`0x00` marker), so a DESC scan returns them last. This matches MySQL, where NULLs are the smallest values.
- LOW: An empty tree or interval: the reverse iterator is immediately done.
- LOW: Merge joins with `IsReversed` use the same `IsReverse` lookups, so they need no extra work.

## Assumptions

- go-mysql-server signals direction only through `IndexLookup.IsReverse` on the lookup passed to `LookupPartitions`/`IndexedAccess`. A full-table `ORDER BY pk DESC` arrives as a reverse lookup over the full range, not as a plain table scan.
