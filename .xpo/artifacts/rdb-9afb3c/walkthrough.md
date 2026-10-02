# Walkthrough: derived journal index edits drop a unique value that moved to another row

Fixed on the rdb-40ba7c branch and merged with it; see that walkthrough for context.

## The bug

`tableState.overlayIndexEdits` derives a persisted index's pending edits from the pending row overlay. It walks the rows in primary-key order and emits, per row, a removal of the base row's index key and an addition of the new one. The last edit per key wins.

For a unique index, the key is the column values without the primary key, so two rows can map to the same key. Take a checkpointed `(1, u=10), (2, u=20)`, then `DELETE id=2` and `UPDATE id=1 SET u=20`:
- row 1 emits a removal of `10` and an addition of `20 → 1`;
- row 2 emits a removal of `20`, which comes later and so wins.

An engine that derives these edits, rather than getting them from the per-generation cache (a freshly opened engine, or after a cache miss), lost `u=20`. A lookup found nothing, and `INSERT … u=20` was accepted, violating the unique constraint. Engines that built the cache incrementally were unaffected, because statement order is causal.

## The fix

For each index, removals are emitted first and additions are collected separately and appended after (`added` map). In the final state at most one row holds a given unique key, so its addition must win over any former holder's removal. Non-unique keys include the primary key and never collide, so their order doesn't matter.

## Test

`TestJournalDerivedUniqueIndexFollowsMovedValue` (`engine/journal_index_test.go`) runs the sequence above, opens a fresh engine, and checks both the lookup and the duplicate rejection. The randomized `TestJournalRebaseMatchesModel` originally found it.
