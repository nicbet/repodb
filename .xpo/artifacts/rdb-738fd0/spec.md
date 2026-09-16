# Prolly tree prefix iteration

## What
Add `IteratorFrom(ctx, startKey)` to the prolly tree — a lazy ordered iterator positioned at the first entry whose key is >= `startKey`.

## Why
Non-unique secondary indexes (rdb-4bdf5d) encode keys as `indexedColumns || PK`. Looking up all rows matching a set of column values requires finding all entries sharing a key prefix. The tree currently only supports `Get(key)` (exact match) and `Iterator()` (full scan from start). Without seek, the only option is a full scan — unacceptable for index lookups.

## How

### 1. `Tree.IteratorFrom(ctx, startKey)` — `tree.go`

Add a new method that returns an `*Iterator` positioned at the first entry >= `startKey`.

**Algorithm** (mirrors `Get`'s navigation but builds the iterator stack):

```
descendTo(hash, startKey):
  read node
  if leaf:
    binary-search for first entry >= startKey
    set it.leaf, it.index
    return
  else:
    binary-search children for first child whose MaxKey >= startKey
    if none found: mark done, return
    push iteratorFrame{node, next: childIdx+1}
    descend into children[childIdx]
```

This is essentially the same B-tree descent as `Get`, but instead of returning a single value, it builds the iterator's stack so `Next()` can continue forward from that position.

### 2. No changes to `Iterator`, `Next()`, or `advance()`

The existing `Next()`/`advance()` logic already handles walking forward through the tree from whatever position the stack represents. `IteratorFrom` just sets up the initial position differently than `Iterator`.

### 3. Tests — `tree_test.go`

- Seek to exact key → returns that entry
- Seek between keys → returns next entry
- Seek past all keys → returns done immediately
- Seek to empty prefix → equivalent to `Iterator()` (returns first entry)
- Seek on empty tree (no entries) → returns done
- Seek on single-entry tree
- Prefix scan pattern: insert entries with shared prefixes, seek to prefix, iterate while prefix matches

## Acceptance Criteria

- [ ] `IteratorFrom` returns correct first entry for exact match, between-keys, and past-end cases
- [ ] `Next()` works correctly after `IteratorFrom` — walks remaining entries in order
- [ ] Empty tree returns done immediately
- [ ] Performance: descends O(log N) nodes, same as `Get`
- [ ] Existing `Iterator` and `Get` behavior unchanged
