# Walkthrough: journal writes missing from persisted secondary indexes

## The bug

In journal mode, once a table's secondary indexes had Prolly trees in Git (true for every indexed table after its first checkpoint, native-git or journal), rows written by *earlier* journal transactions were invisible to index lookups and UNIQUE checks. `SELECT … WHERE indexed_col = ?` missed them, and duplicate unique values were accepted. Full scans and primary-key lookups were fine.

## How journal reads work

A journal-mode snapshot is a Git commit (the **base**, with data and index trees) plus **pending typed row edits** from the journal. At transaction start the edits are decoded into `tableState.edits`, an overlay that `ensureRows` and `lookupRow` apply on top of the base data tree.

Secondary indexes have their own overlay, `tableState.idxEdits`: per index, an ordered list of `prolly.Edit`. Readers consult it before the base index tree, with the last edit per key winning and deletes hiding tree entries. The editor appends to it on every insert, update and delete.

## Root cause

`ensureIndexEdits` builds `idxEdits` lazily. For indexes **without** a base tree it rebuilt entries from `ensureRows`, overlay included, which was correct. For indexes **with** a base tree it started empty. The row overlay from earlier transactions never made it into the index overlay; only the current transaction's editor edits did.

## Fix, part 1: derive index edits from the row overlay

`ensureIndexEdits` now splits indexes into persisted and unpersisted. For persisted ones, `overlayIndexEdits` walks the pending row edits in sorted key order. For each, it reads the base row from the data tree and emits:
- `Delete(oldIndexKey)` if a base row exists;
- `Insert(newIndexKey → pk)` unless the edit is a delete;
- nothing when old and new keys are identical.

Unpersisted indexes keep the full rebuild. No reader changes were needed.

This is safe against double counting because `editor.StatementBegin` initializes `idxEdits` before any write in a statement. So at derivation time `s.edits` holds only overlay rows that have no index edits yet. Even if that ever changed, deriving "overlay vs base" is always the correct index state.

## Fix, part 2: a per-generation cache

Deriving costs one base-tree read per pending edit on every transaction. With 10,000 pending edits a single-row insert took 630 ms (27 ms before the fix, which was fast only because it skipped the work).

The key observation: a committing journal transaction already holds the exact index edits for the generation it creates. A journal commit keeps the base Git commit, so the base trees are unchanged. So:
- `database.indexEdits` is keyed by (commit, generation).
- `storeIndexEdits` runs after a successful `commitTypedEdits`. It caches each table's `idxEdits`, compacted to the last edit per key in key order so the lists don't grow without bound. Tables with schema changes or drops are skipped. Untouched tables carry over their entries if the cache was at the transaction's base.
- `rememberIndexEdits` runs when a clean (read-only) transaction ends. It adds what the transaction derived, so repeated reads after a restart don't re-derive.
- At transaction start, a cache hit hands the transaction `shareIndexEdits(…)`: a new map over the cached slices with capacity clipped (`s[:len:len]`). Appends then reallocate instead of writing into shared memory.

Anything else misses and falls back to derivation: a checkpoint (new commit), another process's generation, or a fresh engine. Result: 32 ms at 10,000 pending edits, with no measurable difference at 1,000 or fewer.

## Tests (`engine/journal_index_test.go`)

- Insert, update, delete and mixed persisted/new-index regressions on a table checkpointed through the engine. All failed before the fix.
- `TestJournalIndexesMatchModel`: 150 deterministic random insert/update/delete/checkpoint/reopen steps. After each step every probed unique and non-unique lookup is compared with an in-memory model. It fails immediately on the old code, and passes with the cache and with cache reads disabled.
- `BenchmarkJournalIndexedInsertWithPending`: the cost guard for the numbers above.

The model test was first written with two engines on one repo. That exposed rdb-bc42f6: readers keep stale snapshots when another process writes, because `StateSeq` is per-process. It's pre-existing, so the test uses reopens instead of a second engine.

## Repo-wide lint (user request)

- gofmt applied to five files that were unformatted on main.
- staticcheck U1000: removed dead `validateManifest`, `sortedHashes`, `cloneObjects`, `indexState`, `cloneRows`, `validateEntries`, `externalMySQLPointReadWrite`, `cleanupExternal`.
- staticcheck ST1005: lowercased `git ref not found`, `git ref changed`, `prolly node … not strictly ordered`.
- New `make lint`: gofmt check, `go vet` and staticcheck v0.8.1 (installed into `bin/`), each native and `GOOS=windows`. Listed in the README's Development section.
