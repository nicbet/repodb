# Prolly tree prefix iteration

## What was built

Added `Tree.IteratorFrom(ctx, startKey)` to the prolly tree — a lazy ordered iterator that positions itself at the first entry whose key is >= `startKey`. This enables efficient prefix scans needed by non-unique secondary index lookups (rdb-4bdf5d).

## How it works

The implementation mirrors `Get`'s B-tree descent but builds the iterator stack instead of returning a single value.

**`seekTo(hash, startKey)`** navigates from root to leaf:

1. At each internal node, binary-search `Children[].MaxKey` to find the first child whose max key >= `startKey`. Push an `iteratorFrame` with `next` pointing to the *following* child (so `advance()` picks up there later).
2. At the leaf, binary-search `Entries[].Key` for the first entry >= `startKey`. If found, set `it.leaf` and `it.index`. If the target falls past the leaf's last entry, fall through to `advance()` to find the next leaf.
3. If no child's `MaxKey` reaches `startKey`, mark done immediately.

After positioning, the existing `Next()` and `advance()` work unchanged — they walk forward through the iterator stack from whatever position `seekTo` established.

**Performance**: O(log N) node reads for the seek, same as `Get`. Each level reads exactly one node.

## Key decisions

- **Reuse `Iterator` struct** rather than creating a separate type. The only difference is how the initial position is established (`descend` vs `seekTo`); forward iteration is identical.
- **`seekTo` calls `advance()` on leaf exhaustion** — when `startKey` is past all entries in the target leaf (possible at chunk boundaries), the iterator naturally moves to the next leaf. This avoids duplicating the parent-traversal logic.

## Files changed

| File | Change |
|------|--------|
| `common/prolly/tree.go` | Added `IteratorFrom` (public) and `seekTo` (private) |
| `common/prolly/tree_test.go` | 8 new tests: exact key, between keys, past end, empty tree, empty start key, single entry, walk remaining entries, prefix scan pattern |
