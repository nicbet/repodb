# Walkthrough: mixed boundary hash and retuned Prolly chunks (format 6)

## What was built and why

The issue was filed because a 50k-row bulk publish wrote ~20% more Git objects after rdb-fb3d12's content-local boundary rule (767 nodes instead of 638). The suspected cause was the chunk options. A simulation over 50k keys of several shapes showed the real cause was the **hash**:

- The rule tests the low 6 bits of FNV-64a(key). FNV's last step is `(h ^ lastByte) * prime`, and the low bits of a product depend only on the low bits of its factors. So the hash's low bits are a function of the last byte's low bits.
- Sequential BIGINT keys (8-byte big-endian) differ in the last byte, so a boundary fell exactly every 64 keys, whatever the options. Raising the minimum to 64 left BIGINT leaves at 67.5 entries while string keys reached 124.
- At the old 32/128 options, 20–24% of chunks also ended at the size cap. A cap cut depends on position, not content, which weakens edit locality.

The fix is two lines of substance, plus a format bump:

1. `keyBoundaryHash = splitmix64-finalizer(FNV-64a(key))`, used for leaf keys and link max keys.
2. `DefaultOptions = {Min 64, Max 256, BoundaryBits 6}`: about 124 entries per leaf for every key shape, with about 4% cap cuts.
3. `FormatVersion` 5 → 6, `snapshotValidationVersion` 2 → 3, no migration.

## Results (Docker, p50, 50k rows, main → branch)

- `bench` leaves: 742 → 399.
- **Native-git** improved everywhere:
  - single-row update 18.7 → 14.9 ms;
  - batch of 100: 22.4 → 17.6 ms;
  - full scan 17.6 → 12.9 ms;
  - bulk load 757 → 320 ms;
  - divergent merge 380 → 314 ms;
  - edit/sync round trip 167 → 131 ms.
  Each commit writes fewer Git objects because the tree is shallower, and that outweighs rewriting a bigger leaf.
- **Journal** `initial_publish_sync`: 793 → 568 ms. Divergent merge 655 → 552 ms.
- **Reads** are unchanged within noise.

## Key decisions

- **A finalizer, not a new hash.** splitmix64's finalizer is a few multiplies and xor-shifts. It makes every output bit depend on every input bit, which a low-bit test needs. It keeps FNV's byte loop and needs no dependency.
- **64/256, not 48/192.** The largest average with rare cap cuts. The feared cost (bigger leaves per single-row native-git write) measured as a gain.
- **The interior levels** use the same rule on link max keys, so fan-out rose and trees got shallower.

## Tests

`TestChunkSizesAreKeyShapeIndependent` builds 50k sequential BIGINT, string and random keys with `DefaultOptions`. It requires average leaf sizes within 25% of each other and under 10% of leaves at the cap. The old hash fails it, because BIGINT sits at 67 against ~124. The history-independence, edit-locality and sorted-builder tests pass unchanged.

## Also in this change: rdb-6bd369

While measuring, an in-process bulk load (50k rows, one transaction) turned out to be 20% slower on `main` than at `062f8b3`. The profile pointed at `PendingRows.With` (rdb-a6a4d2): `sort.SliceStable` over 56-byte edits does O(n log² n) rotations. `With` now sorts an `[]int32` permutation with `slices.SortFunc`, ties broken by position, which gives the same order as a stable sort. Bulk load went 166 → 140 ms.

## Non-obvious

- **Ignore single-sample journal workloads in Docker.** `publish_sync_after_writes` and `bulk_load` (n = 1) varied by up to 2× between runs of identical code. Use the in-process benchmarks for those.
