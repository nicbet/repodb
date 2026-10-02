# rdb-a8200b walkthrough: validation skips unchanged subtrees

## The problem

`engine.ValidateSnapshot` already skipped tables whose roots were unchanged (rdb-187370, rdb-93103f). But one changed row gives a table new roots, and the table was then validated from scratch:
- `validateTableRows` decoded every row with an `Iterator`;
- `prolly.Reachable` walked every data and index node again.

Prolly trees are content-addressed, and an update rewrites only one leaf and its path to the root. Almost all of that work repeated checks on nodes whose hashes had already been validated.

## The idea

Validity of a Prolly subtree depends only on its content, which its hash names, plus, for data trees, the schema the rows are decoded under. So we can remember "this node's subtree is valid" by hash, and a parent that links to a remembered node can skip it. The parent still checks that the child's entry count matches its link.

## Pieces

### `prolly.Validate` (`common/prolly/tree.go`)

`walk` became a `walker` struct:
- `store`;
- `seen` (hash → subtree count; it dedupes and becomes the returned hash list);
- an optional `NodeCache`;
- an optional `visit`.

`Validate(ctx, store, root, cache, leaf)`:
- checks the root against the cache first;
- at each internal node, asks the cache about every unseen child **before** `prefetchChildren`. Cached children land in `seen`, so they are filtered out of the prefetch batch, and the child loop finds them in `seen` and only checks counts. Asking at the parent rather than at the top of `walk` expands each cached subtree only once;
- on a miss, validates as before: ordering, link validity, counts. Then it calls `leaf(entries)` for leaves, records `seen`, and `Remember`s the node with its count and child hashes. Children are always remembered before parents.

`Reachable` is `Validate` with no cache or callback, so the checkpoint, merge and catalog fallbacks behave exactly as before (decision D2). `Tree.Entries` uses the same walker with `seen == nil`.

`NodeCache.Subtree` returns *every* node of the cached subtree with its count, not only the root. Callers need the complete reachable object list: the table cache records it, and checkpoint, merge and catalog copy those objects. A cache hit must therefore still be able to enumerate the skipped nodes, in memory and without reading them.

### Row checks inside the walk (`engine/engine.go`)

`validateTable` loads the schema once and passes a `leaf` callback (`validateRows`) to `prolly.Validate`. The callback decodes each row and checks that its primary key matches the stored key, as `validateTableRows` used to. The separate iterator pass is gone, so a cold validation also reads each node once instead of twice. Error messages lost the "reachability" suffix, because structure and row errors now come from the same call.

### The node cache (`validatedNodes`, `snapshotNodeCache`)

A process-wide LRU keyed by `identity \0 version \0 scope \0 hash`:
- **scope = schema root** for data trees. A leaf whose rows decode under schema A may not decode under schema B, so DDL starts from an empty scope and re-decodes every row (tested).
- **scope = "index"** for index trees, which need only structural checks. Index entries are shared across tables and schemas, and a data lookup can never match them.

Each entry stores the `ValidatedNode` and the `repository.ObjectRef` (hash and Git OID) the node was read under.

`snapshotNodeCache` adapts the LRU to one snapshot and one scope:
- **`Subtree`** does a DFS through cached `Children` under the lock. It misses if any descendant has been evicted. Outside the lock it requires `snapshot.Provides(refs)`, the same trust rule as the table cache. So a fetched commit that stores a cached node hash under a different blob is re-read and rejected with `ErrCorrupt` (tested).
- **`Remember`** records the node with the snapshot's OID for it (empty for journal-only objects; `Provides` then accepts it only if the bytes are in that snapshot's checked cache).

**Eviction** is LRU by size = 1 + number of children, with a cap of 2^20 (decision D1: its own budget, separate from the table cache's). Evicting a child while its parent stays cached is safe: the parent's `Subtree` misses, the walker reads the parent, then asks about each child individually. A small cache costs more work but never gives a wrong answer (tested with a cap of 8).

`snapshotValidationVersion` went to 6, which retires any cache entries from older code within a process.

### Counters

`NodesValidated` counts `Remember` calls. `NodesReused` counts nodes returned by cache hits. The engine tests use them, together with `RowsDecoded`, to assert that a one-row checkpoint of a 10k-row indexed table decodes at most one leaf and validates at most 12 nodes.

## Results

dbbench `mutable` at 100k rows, 3 growth rounds, Linux container. `ValidateSnapshot` CPU in growth sync fell from 280 ms to 60 ms (~93 → ~20 ms per round). The spec targeted "a few ms". The remainder is real work: each round's 600 scattered edits rewrite a large share of the ~800 leaves. The user accepted this. Peer pull is unchanged, because a cold process has an empty cache. Per-round sync time is within noise.

## Things to know

- The cache is per process. A CLI run or a fresh peer validates fully once, as before.
- Validated nodes are remembered even if validation of the table fails later on another subtree. Each remembered node is valid on its own, so this is sound.
- `Provides` is checked once per cached subtree and takes the snapshot cache's read lock, which is cheap compared with reading nodes.
