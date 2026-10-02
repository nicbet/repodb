# Prolly chunking: mixed boundary hash, retuned sizes (format 6)

## What

Prolly chunk boundaries test the low bits of `mix(FNV-64a(key))`, using the splitmix64 finalizer, instead of FNV-64a's raw low bits. `DefaultOptions` become `MinChunkEntries: 64, MaxChunkEntries: 256, BoundaryBits: 6`. The storage format goes 5 → 6.

## Why

FNV-64a's low bits depend only on the low bits of the key's last byte, so sequential integer keys get a boundary exactly every 64 keys, whatever the minimum. That's 742 leaves for the 50k-row BIGINT table, ~20% more objects than a well-mixed hash gives at the same options. At today's 32/128 options, ~20% of chunks also end at the size cap, which depends on position rather than content and weakens edit locality. See the issue for the simulation.

## Acceptance Criteria

- The 50k-row BIGINT `bench` table has 350–450 leaves.
- The simulation shape holds in a unit test: average leaf size for sequential BIGINT, string and random keys within 25% of each other, and cap cuts under 10%.
- The existing Prolly tests pass unchanged: history independence, edit locality (one node per level per update, a small constant per insert or delete), and the sorted builder matching `Build`.
- Measured before and after, recorded in the issue:
  - journal `initial_publish_sync` at 50k rows (`make bench-docker`);
  - `BenchmarkSQLAutocommitScans`;
  - the native-git single-row update (`BenchmarkSQLExactKeyUpdate` or the scorecard's).
- `docs/architecture.md` describes the new rule and format 6.

## Flow

1. **`common/prolly/tree.go`**: `entryBoundaryHash` and `linkBoundaryHash` return `mix64(fnv64a(key))`, where `mix64` is the splitmix64 finalizer. Set the new `DefaultOptions`.
2. **`repository.FormatVersion = 6`**; bump `snapshotValidationVersion`.
3. **Tests**: a chunk-shape test over the three key shapes, plus re-running the existing suites. Fix any test that hard-codes node counts from the old rule.
4. **Measure, then update the docs**: `architecture.md`'s chunking paragraph (32 → 64, 128 → 256, mixed hash) and the format version.

## Decisions

- **A finalizer over FNV, not a new hash.** splitmix64's finalizer is a few multiplies and xor-shifts. It makes every output bit depend on every input bit, which is exactly what a low-bit test needs, and it keeps FNV's byte loop. xxhash or SHA-256 would also work but add cost or a dependency for no measured gain.
- **64/256/6 bits.** It gives the largest average (~124) with rare cap cuts (~4%). 48/192 gives ~106 with ~10% cap cuts. Bigger leaves mean fewer objects per bulk publish and checkpoint, and better locality, against more bytes rewritten per single-row update. Reads are unaffected: an in-place leaf scan of 124 entries is cheap.
- **Interior levels use the same rule** (link max keys), so the fan-out also rises, and trees get shallower.

## Edge Cases

- MEDIUM: Native-git single-row writes rewrite a leaf of ~124 entries instead of ~67, so more bytes are hashed and written. If the measured regression exceeds ~20%, fall back to 48/192.
- LOW: Merge and history independence don't depend on the options, since the rule stays content-local; the existing tests cover both.

## Assumptions

- Changing chunking only needs a format bump (alpha, no migration), like rdb-fb3d12.
