# rdb-4d8ccb walkthrough: binary-searching Prolly nodes through cached offsets

## The problem

Secondary-index scans (`indexRowIter` in `engine/ranges.go`) fetch each matching row through `tableState.lookupRow` → `prolly.(*Tree).Get`. At 100k issues, the dbbench assignee query (3,750 matches) took about 33 ms, and it capped `issue_mixed_concurrent` at about 300 ops/s with one client.

## Finding the real cost (and a wrong first idea)

The issue's first theory was the repeated root-to-leaf descent: interior nodes are scanned link by link. The first spec proposed a cursor that kept its path between lookups, since index order yields ascending PKs within a prefix. The prototype made things **worse** (48 vs 34.5 ms/op).

A Linux CPU profile of a dedicated benchmark showed why. 1.65 s of `Get`'s 2.0 s was the **leaf** loop: `nextEntry` plus `bytes.Compare`. With `DefaultOptions` a leaf holds 64–256 entries, here about 160 rows of about 600 bytes (around 96 KB). Entries are length-prefixed, so `Get` must parse every entry before the key, taking roughly one cache miss per entry. The cursor decoded every leaf it visited. Matches are sparse in PK order (about 2 per leaf per index prefix), so that decoding cost as much as the scans it replaced. No traversal strategy avoids walking a leaf. Only knowing where each entry starts does.

## The fix

**`common/prolly/offsets.go`**

- `itemOffsets(r nodeReader)` returns the start offset of each item in a node. On a miss it walks the node once (`nextEntry` for leaves, `nextLinkRaw` for interior nodes), requires `finish()` (no trailing bytes), and caches the result.
- The cache is keyed by **node hash**. Nodes are content-addressed and immutable, so offsets depend only on the hash: they can't go stale and are valid in every store, snapshot and transaction. That is why the cache can be process-wide.
- It is bounded: 16 shards, each a `sync.RWMutex` plus a map holding at most 4096 entries. A full shard evicts an arbitrary entry. That keeps a hit to one `RLock` and a map read. Each entry is one `uint32` per item (about 640 bytes for a typical leaf). The node bytes stay in the store's own cache.
- `nodeReader.search(key)` binary-searches the offsets, parsing only the probed items' key fields. It returns the first index whose key is `>= key` and a reader positioned on that item (`at(offset)` sets `pos` and `remaining = 1`, so the normal `nextEntry`/`nextLinkRaw` parsers and their bounds checks apply).

**`Tree.Get`** (`common/prolly/tree.go`) calls `search` at every level. If no item qualifies, it returns `ErrNotFound`. In a leaf, an equal key returns `clone(value)`; otherwise `ErrNotFound`. In an interior node it follows the link. That is about 8 key probes per leaf instead of about 80 entry parses.

One behavioural nuance: the old `Get` stopped parsing at the key, so a corrupt tail past it went unnoticed. Now the first `Get` of a node checks its whole framing. Since every `Get` caller benefits (index scans, PK point reads, unique checks, joins), there are no caller changes.

## Tests and measurement

- `common/prolly/get_test.go`: on trees of 1, 2, 255 and 20k entries (odd keys stored), `Get` returns the iterator's value for every stored key, and `ErrNotFound` for keys before, between (even, and extended keys) and after. Hand-built leaves with trailing bytes or a truncated item fail with a decode error, not `ErrNotFound`.
- `BenchmarkSQLAutocommitIndexScan` (`engine/scan_bench_test.go`): a checkpointed 100k-row journal table shaped like dbbench's `issues`. Status is `(id/20)%4`, so it isn't correlated with `id%20` assignees. The first fixture made them correlated and the query returned the wrong row count, which the benchmark checks. Result: 34.5 → 9.6 ms/op.
- Docker `mutable` at 100k: `issue_assignee_query` went from 30.5 to 66.4 ops/s, and `issue_mixed_concurrent` from 255 to 518 (1 client) and from 911 to 1,202 (16 clients). The existing point-read and scan benchmarks are flat. An apparent 1k point-read slowdown was noise; alternating re-runs showed none.

## Not done / future

- An on-disk offset table in the node format would remove even the first-touch parse, but it is a format change.
- If working sets grow past about 64k nodes, LRU or a size-aware bound may beat arbitrary eviction.
