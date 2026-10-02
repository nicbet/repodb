# Walkthrough: per-key write conflict detection with commit rebase (journal)

## What was built and why

Until now a journal commit failed with `ErrConflict` whenever the journal generation had moved since the transaction's snapshot, even if the other commits touched unrelated rows. In `mixed_80read_20write`, where each client updates its own row, 4–18% of writes were rejected for no reason. Journal commits are now **first-committer-wins snapshot isolation**: a transaction commits on top of whatever was committed since its snapshot, unless its writes overlap.

Native-git mode and the untyped `WorkingState.Commit` keep the strict rule.

## The key insight: the rebase was already free

Since rdb-a6a4d2, a journal transaction appends only its **own** row edits, and `applyTypedEditsToSnapshot` applies them to `view.snapshot`, which is the *current* state, not the transaction's base. So once the generation check is relaxed, "rebasing" a disjoint transaction costs no new code. The real work was:

1. defining "disjoint" precisely, and recording enough history to check it;
2. keeping the engine's per-generation index-edit cache correct after a rebased commit.

## Conflict detection (`common/repository/writelog.go`)

`workingView.writeConflict(base, edits)` runs under `working.lock` in `CommitTypedEdits`, after `load`:

- **Checkpoint since the snapshot**: `base.Commit != view.snapshot.Commit` gives a conflict. Rebasing across checkpoints is out of scope.
- **Rows**: each pending edit now carries the generation that wrote it (`TypedRowEdit.generation`, unexported, so it is never journaled). It is stamped by `PendingRows.with(edits, generation)`. The pending overlay is exactly "the newest edit per key since the base commit", so `pending.Get(key).generation > base` answers "was this row written after my snapshot?". No second per-key map is needed, and the extra memory is 8 bytes per pending edit.
- **Unique indexes**: two transactions inserting different primary keys with the same unique value would both pass the row check. The engine therefore journals `TypedTableEdit.Claims`: the keys this transaction *added* to unique indexes (`uniqueClaims()`, from each unique index overlay's `local` edits, last edit per key). A non-NULL unique index key is the column values without the primary key, so equal values give equal keys. Keys with a NULL include the primary key and never collide, which matches MySQL. Deletes need no claim: a transaction can only free a value its snapshot saw taken, and no concurrent transaction could have claimed that value against a snapshot where it was taken.
- **Schema**: `writeLog.tables` keeps per table the last generation that created, altered or dropped it (`schema`) and the last that touched it at all (`touched`). A write to a table whose schema changed after the snapshot conflicts, and a schema change or drop of a table touched after the snapshot conflicts.

The write log (`tables` and `claims`) lives on `workingView`. It is rebuilt by replay (`materialize`), which is what makes commits from **other processes** visible to the check. It is reset (set to nil) wherever the pending overlay is reset: checkpoint, base change, an untyped `prepare` (a whole-manifest commit) and `reconcileHead`. A nil log is created lazily with `from = generation - 1`, and a snapshot older than `from` always conflicts, because the log doesn't cover the gap.

The log is a shared pointer, mutated in place under the lock. Entries are only ever added with increasing generations. So if a view shares a log that a later (possibly failed) load extended, the worst outcome is a spurious conflict, never a missed one.

## Engine: the index-edit cache (`storeIndexEdits`)

The engine caches each generation's derived secondary-index edits (rdb-e472aa), because deriving them costs one base-tree read per pending edit. A committing transaction used to store its `idxEdits` as the next generation's cache. After a rebase that would be wrong: those views were built at the transaction's base and miss the intervening commits.

A commit is rebased when `next.Generation() != base.Generation()+1`. In that case, if the cache holds generation `next-1`, each dirty table's cached views are extended with the transaction's own `overlay.local` edits (`rebaseIndexViews`), and clean tables carry over. This is correct because the transaction's local index edits are "delete the old key, add the new key" for rows it wrote. Those rows are disjoint from every intervening write, so the old values it saw are still current. If the cache isn't at `next-1` (for example, another process committed in between), the cache is left alone: a later miss, never a wrong answer. This avoids paying a full re-derivation, about 630 ms at 10k pending edits, on every rebased commit.

Two traps found during implementation:
- A clean table's `idxEdits` (derived for reading during the transaction) are of the *base* generation, so for a rebased commit only carried views may be cached.
- A nil views map must never be stored. `StartTransaction` would turn it into an empty non-nil `idxEdits`, `ensureIndexEdits` would then skip derivation, and pending rows would vanish from index lookups.

## Bug found along the way: rdb-9afb3c

The randomized model test (`TestJournalRebaseMatchesModel`) exposed a bug that predates this change, in `overlayIndexEdits`. It derives index edits from pending rows in primary-key order, emitting a removal and then an addition per row. When a unique value moved from row 10 (deleted) to row 1 after a checkpoint, row 1's addition came before row 10's removal, and the removal won. A fresh engine then lost the value, and also accepted a duplicate insert. The fix emits each index's removals before its additions. In the final state at most one row holds a unique key, so its addition must win.

## Crash safety

The journal records are unchanged in shape (`typed-prepare` + `commit` at `view.generation + 1`), plus the optional `claims` field. A rebased commit is recovered, replayed, compacted and answered by `RecoverTransaction` exactly like any other. The format version stays 3, because the field is additive (`omitempty`).

## Guarantee

This is snapshot isolation, not serializability: reads aren't tracked. Write skew is possible (each transaction reads a condition and writes a *different* row). `docs/sql.md` documents it with the guard-row workaround. Lost updates are still impossible: same-row writers conflict.

## Measurements (dbbench, journal, 50k rows, one run on macOS)

The mixed workload went from 29 and 189 conflicts (4 and 16 clients) to 0. Wall time and p50 for the mixed workload rose, because rejected writes used to fail fast without a flush, while every write now appends and flushes. Successful writes per second rose from about 1.9k to about 2.9k. Group commit (rdb-df092b) is the lever for that remaining cost. `contended_increment` still passes its lost-update check.

## Tests

- `common/repository/writelog_internal_test.go`: a conflict matrix (rows, deletes, unique claims across rows, indexes and tables, schema, drop, create), rebase across generations committed by another `WorkingState`, a full replay still detecting conflicts, coverage rules, and faults before the append and after the flush, plus a torn tail.
- `engine/journal_rebase_test.go`: SQL-level disjoint, same-row (including a primary-key change), unique/NULL, DDL, checkpoint and cross-engine cases; plus the randomized model test (60 rounds × 3 concurrent transactions, one or two engines). The model asserts that a commit is rejected **exactly** when it overlaps an earlier commit in its round, and that unique and secondary index lookups match the model each round.
- Mutation checks: caching stale views, or dropping claims, each fail the new tests.
- Existing tests that used disjoint stale writers now write the same row, so they keep testing rejection.
