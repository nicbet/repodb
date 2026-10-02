# rdb-a8200b: Validation skips unchanged subtrees of a changed table

## What

When a table's roots change, `engine.ValidateSnapshot` validates only the Prolly nodes it has not already validated under the same schema. It still returns, and caches, the table's full reachable object list. Unchanged subtrees are neither read, decoded nor re-walked.

## Why

Today a one-row change re-validates the whole table: every row is decoded and key-checked (`validateTableRows`), and every data and index node is walked by `prolly.Reachable`. At 100k issues that is ~0.1 s CPU per checkpoint. With lazy reads (rdb-93103f) it can also read untouched nodes from Git. A one-row update rewrites one leaf and its path to the root (~3–4 nodes per tree), so almost all of that work is repeated.

## How

### 1. One walk per tree, with an optional node cache (`common/prolly/tree.go`)

Add an exported walk that validates a tree, calls a leaf callback, and consults a caller-supplied cache of validated nodes:

```go
// ValidatedNode is a node whose subtree has been validated.
type ValidatedNode struct {
    Count    uint64         // leaf entries in the subtree
    Children []storage.Hash // direct children; nil for a leaf
}

type NodeCache interface {
    // Subtree returns every node of hash's subtree, root first, each with its
    // subtree's entry count, or false if any node of it is unknown or unusable.
    Subtree(hash storage.Hash) ([]NodeCount, bool) // NodeCount{Hash, Count}
    Remember(hash storage.Hash, node ValidatedNode)
}

func Validate(ctx, store, root, cache NodeCache, leaf func(entries []Entry) error) ([]storage.Hash, error)
```

- `walk` checks `cache.Subtree` for the root and, at each internal node, for every child before prefetching (*revised during implementation*: per child, so cached children are never prefetched and each subtree is expanded once). On a hit it records the subtree's nodes in `seen`, returns the cached count (the parent still checks it against its child link), and does not read, prefetch or call `leaf`.
- On a miss it validates exactly as today, then calls `leaf` for leaves and `Remember` once the node's subtree is fully validated (children before parent).
- `Reachable` becomes `Validate(ctx, store, root, nil, nil)`: no behaviour change for its other callers (checkpoint, merge, catalog fallbacks).

### 2. Row checks move into the walk (`engine/engine.go`)

`validateTableRows` stops streaming a separate `Iterator`. The data tree is validated by one `prolly.Validate` call whose `leaf` callback decodes each row and checks its key, as now. Index trees use the same call with no `leaf` callback. Checks are unchanged: per-node ordering, child-link ordering and counts, row decode, primary-key match.

### 3. Process-wide validated-node cache (`engine/engine.go`)

An LRU keyed by **(repository identity, `snapshotValidationVersion`, scope, node hash)**:
- **scope = schema root** for data trees: a leaf valid under one schema may not be after DDL;
- **scope = "index"** for index trees: structural checks only, no schema. A data-tree lookup never matches an index-scope entry.

Each entry holds the `ValidatedNode` plus the Git OID the node was read under (`repository.ObjectRef`, as rdb-93103f).

`ValidateSnapshot` wraps the LRU in a per-snapshot `NodeCache` adapter whose `Subtree`:
- expands the subtree in memory through cached `Children` (no object reads);
- misses if any descendant has been evicted;
- misses unless `snapshot.Provides` every node's `ObjectRef`, the same trust rule as the table cache. So a journal-only node, or one stored under another blob in a fetched commit, never vouches for a snapshot that lacks it.

A miss on a subtree root falls back to reading it and looking up its children one by one, so eviction degrades to more work, never to a wrong answer.

The table-level cache stays as the fast path for tables whose roots did not change, and still records the full object list (now assembled from walked and cached nodes).

**Bound:** at most 2^20 recorded child links + nodes across entries, LRU (≈ 100 MB worst case at 64-hex-char hashes). At 100k rows a table is ~800 leaves, so this covers hundreds of large tables.

`snapshotValidationVersion` goes to 6.

### 4. Counters and docs

- Add `nodesValidated` and `nodesReused` to `performanceCounters` (and the public counters struct).
- `docs/architecture.md`, "SQL validation": describe the node cache, its scope rule, the OID pinning and the bound. Remove the rdb-a8200b mention if any.

## Edge cases

- **DDL (schema root changes):** every data node misses → full re-validation, as today. Index trees whose roots are unchanged still hit.
- **New index on an existing table:** new tree, all misses.
- **Corrupt node under a cached parent in a fetched snapshot:** if the fetched snapshot maps it to another OID, `Provides` fails, the subtree is re-read and the corruption is reported.
- **Same node hash in two tables with the same schema root:** shared — correct, because validity depends only on the content and the schema.
- **Empty tables / no data root:** unchanged.
- **Concurrent validations:** the LRU is mutex-protected; duplicate `Remember` is idempotent.

## Acceptance criteria

- [x] After validating a 10k-row table, updating one row and validating again decodes ≤ one leaf's rows (`rowsDecoded`) and walks only the changed path (`nodesValidated`).
- [x] A schema change re-decodes every row.
- [x] A fetched snapshot whose unchanged leaf maps to a different blob is rejected with `ErrCorrupt` even though the node is cached (extends the rdb-93103f test).
- [x] Evicting part of a cached subtree still validates correctly (test with a tiny bound).
- [x] Existing corruption/validation tests pass unchanged; `make test` and `make lint` pass.
- [x] dbbench `mutable` at 100k (Linux container): `ValidateSnapshot` CPU per checkpoint drops from ~0.1 s to a few ms; report before/after. *Outcome (accepted by the user): over 3 growth rounds, 280 ms → 60 ms (~93 → ~20 ms per round). Each round's 600 scattered edits rewrite a large share of the ~800 leaves, so the remainder is mostly content that really changed; the single-row case meets the target (engine test). Peer pull is unchanged (cold process, 240 ms); per-round sync time is within noise.*

## Decisions (confirmed by the user, 2026-10-02)

- **D1: The node cache has its own bound** (2^20 recorded nodes + child links), separate from the table cache's 2^20 objects.
- **D2: The checkpoint, merge and catalog `Reachable` fallbacks stay as they are.** They run only after a table-cache miss, which `ValidateSnapshot` normally fills first.
