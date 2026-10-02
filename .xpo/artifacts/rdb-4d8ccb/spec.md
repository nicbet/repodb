# rdb-4d8ccb: Binary-search Prolly nodes through cached item offsets

## What

`prolly.(*Tree).Get` (`common/prolly/tree.go`) finds a key by parsing every item of each node on the path until it reaches one `>=` the key. Secondary-index scans (`indexRowIter` → `tableState.lookupRow`) call it once per matching row, and the cost is dominated by the **leaf** scan.

Cache each node's item start offsets, keyed by node hash, and binary-search them in `Tree.Get`.

## Why

At 100k issues, `issue_assignee_query` (`WHERE assignee = ? AND status <> 'done'` over `by_assignee (assignee, status)`, 3,750 matches) takes 32.75 ms p50 in dbbench and 34.5 ms in `BenchmarkSQLAutocommitIndexScan`. It is 10% of `issue_mixed_concurrent` and caps it at about 300 ops/s with one client.

In a Linux profile of the benchmark, `Tree.Get` takes 2.0 s of the 2.15 s query time. 1.65 s of that is `nextEntry` plus key comparison in the leaf loop. Leaves hold 64–256 entries (`DefaultOptions`), here about 160 rows of about 600 bytes, so a lookup parses about 80 entries and takes a cache miss on almost every one. Interior link scans cost only about 0.12 s.

### Rejected: a path-reusing lookup cursor (the first version of this spec)

The prototype kept the root-to-leaf path between lookups and decoded each visited node. It ran at 48 ms/op (34.5 before) with 3× the allocations. Matches are sparse in PK order (about 2 per leaf per index prefix), so decoding each leaf costs as much as the linear scans it replaced. Entries are variable-length, so no cursor can avoid walking a leaf. Only an offset table makes binary search possible.

## How

### 1. Offset cache (`common/prolly/offsets.go`)

- Nodes are content-addressed and immutable, so a node's item offsets depend only on its hash and are valid in every store and snapshot.
- `itemOffsets(r nodeReader) ([]uint32, error)` returns the cached offsets. On a miss it parses the node once (`nextEntry` for leaves, `nextLinkRaw` for interior nodes), records each item's start, requires `finish()` (no trailing bytes), and stores the result.
- The cache is process-wide and bounded: 16 shards, each a `sync.RWMutex` plus `map[storage.Hash][]uint32` holding at most 4096 entries. A full shard evicts an arbitrary entry. Each entry is one `uint32` per item, about 640 bytes for a typical leaf. The node bytes are held by the store's own cache, not by this one.
- `nodeReader.search(key)` binary-searches the offsets, parsing only the key field of each probed item (a leaf key, or an interior max key). It returns the first index whose key is `>= key` and a reader positioned at that item.

### 2. `Tree.Get`

At each level: `openNode`, then `search`. If no item qualifies → `ErrNotFound`. In a leaf, read the entry: if the keys are equal, return `clone(value)`, otherwise `ErrNotFound`. In an interior node, read the link and descend. The signature and behaviour are unchanged, so every caller benefits: index scans, PK point reads, unique checks and joins.

Reading a node through `Get` now checks the whole node (item framing and trailing bytes) on first use. The old `Get` stopped parsing at the key. That is stricter, not looser.

### 3. Benchmark

`BenchmarkSQLAutocommitIndexScan` (`engine/scan_bench_test.go`) uses a checkpointed 100k-row journal table shaped like dbbench's `issues`: 20 assignees, 4 statuses not correlated with the assignee, 512-byte bodies, and `INDEX by_assignee (assignee, status)`. It runs the assignee query and checks the row count.

## Decisions

- **Process-wide cache keyed by hash, not per tree or per snapshot.** The offsets depend only on content, so sharing is safe and survives across transactions and journal generations.
- **Arbitrary eviction, not LRU.** It keeps the read path to one `RLock` on a hit. Revisit if a working set larger than 64k nodes shows a low hit rate.
- **No node-format change.** An on-disk offset table would avoid the first parse, but it is a format change.

## Edge cases

- Empty leaf (an empty tree's root): there are no offsets, so `ErrNotFound`.
- A key beyond the last max key in an interior node: `ErrNotFound`.
- Corrupt or truncated node: the first `itemOffsets` call fails with the codec's error, and nothing is cached.
- A probe can't read past the node: offsets come from validated parsing of the same bytes, and `field()` still bounds-checks.

## Acceptance criteria

- [ ] Prolly test: on multi-level trees, `Get` agrees with the iterator for every stored key, and returns `ErrNotFound` for keys before, between and after the entries. A truncated node or one with trailing bytes makes `Get` fail.
- [ ] `BenchmarkSQLAutocommitIndexScan` is at least 2× faster (prototype: 34.5 → 9.6 ms/op). Before/after go in the completion comment.
- [ ] Docker `mutable` run at 100k: `issue_assignee_query` up at least 2×, and `issue_mixed_concurrent` improves to match. Numbers go in the completion comment. `latest.md` is not republished.
- [ ] `go test ./...` passes, and the race run over `common/prolly`, `engine` and `integration` passes.

## Docs

`docs/architecture.md`: describe the Prolly point-read path (an offset cache keyed by hash, plus binary search) where it describes the tree.
