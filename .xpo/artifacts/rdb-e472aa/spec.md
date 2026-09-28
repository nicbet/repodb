# Journal writes skip secondary indexes on tables with persisted index trees

## What

In journal mode, rows written by earlier transactions are invisible to secondary-index lookups, and UNIQUE checks don't see them, whenever the table's indexes already have Prolly trees in the base snapshot. Scans and primary-key lookups do see the rows.

This affects every table with a secondary index after its first checkpoint, whether native-git or engine journal checkpoint, not only tables written in native-git mode. It covers wrong results for index-planned queries and duplicate values accepted into UNIQUE indexes.

## Why it happens

Journal persistence keeps the snapshot's data and index trees as the *base*, and layers pending typed row edits on top. At transaction start they are decoded into `tableState.edits`; `ensureRows` and `lookupRow` apply that overlay.

Secondary-index access goes through `tableState.idxEdits`, built lazily by `ensureIndexEdits` (`engine/catalog.go`):
- For indexes **without** a persisted tree, it rebuilds all entries from `ensureRows`, overlay included, so they are correct.
- For indexes **with** a persisted tree, it starts from an empty edit list, so the overlay rows from earlier transactions never reach the index. Only edits made in the current transaction (via the editor's `addIndexEdit`) are visible.

Checkpoints are unaffected in how they build index trees (`checkpointIndexTrees` rebuilds from the full data tree), but duplicates accepted by the broken UNIQUE check are persisted.

## How

1. **Derive overlay index edits.** In `ensureIndexEdits`, split indexes into persisted and unpersisted. For persisted ones, `overlayIndexEdits` walks the pending row edits in sorted key order and reads each key's base row from the data tree:
   - base row exists → `Delete` its index entry;
   - the edit is not a delete → insert the new entry → PK;
   - identical old and new entries → skip.

   Unpersisted indexes keep the full rebuild from rows. A table can have both kinds.

   Readers (`resolveIndexToPK`, `resolveIndexToPKs`, `lookupIndexKey`) already apply `idxEdits` last-wins with deletes suppressing tree entries, so they are unchanged. `StatementBegin` initializes index edits before any write, so current-transaction edits are never double-counted.
2. **Cache index edits per generation** (`database.indexEdits`, keyed by snapshot commit and generation):
   - A committing journal transaction stores its tables' `idxEdits`, compacted to the last edit per key in key order. They are exactly the index edits for the generation it creates, because a journal commit keeps the base Git commit and its trees. Tables with schema changes or drops are excluded. Untouched tables carry over the previous generation's entries when the cache was at the transaction's base.
   - A clean transaction adds entries it derived that aren't cached yet.
   - At transaction start, cached entries are shared with the transaction with slice capacity clipped, so appends copy instead of writing into the cache.
   - A checkpoint (new commit), another process's generation, or a restart simply misses, and derivation runs again.
3. **Repo-wide lint (user request):**
   - gofmt the five unformatted files;
   - delete eight unused functions/types flagged by staticcheck U1000;
   - lowercase three capitalized error strings (ST1005: `git ref not found`, `git ref changed`, `prolly node … not strictly ordered`);
   - add `make lint` (gofmt check, go vet and staticcheck v0.8.1, each native and GOOS=windows), documented in the README.

## Decisions

- **Derive from the overlay, not a full rebuild.** Rebuilding persisted indexes from all rows would be O(table) per transaction.
- **Cache added (revised from "no cache yet").** Measured single-row insert with 10,000 pending edits on an indexed table: 27 ms before the fix (incorrect), 630 ms with derivation alone, 32 ms with the cache. At ≤1,000 pending edits there is no measurable difference.
- **Repo-wide lint is clean (user request).** Pre-existing fixes are mechanical with no behavior change: dead code removal, formatting, and error-string casing. No test matches those strings.
- **Existing duplicates are not repaired.** Data already corrupted by this bug keeps its duplicates. Detecting and repairing them belongs to rdb-16df08 (corruption detection).
- **Cross-process staleness is out of scope.** It was found while writing the model test: readers keep a stale snapshot when another process writes, because `StateSeq` is per-process. Filed as rdb-bc42f6. The model test therefore uses one engine with periodic reopens rather than two engines.

## Acceptance criteria

Regression tests in the engine, journal mode, for a table whose unique and non-unique indexes have persisted trees (rows checkpointed first):
- An insert in one transaction is found by unique and non-unique index lookups in the next transaction, and after reopening the engine.
- A duplicate unique value is rejected against a row from an earlier journal transaction.
- Update: the old index value is no longer found and the new one is; uniqueness is checked against the new value.
- Delete: the row is no longer found via the index, and its unique value can be reused.
- A mixed table (persisted index plus an index added in the journal) behaves correctly for both.
- Model-based test: 150 random insert/update/delete/checkpoint/reopen steps; every index lookup matches an in-memory model, with and without the cache.

Also:
- The binary repro from the issue behaves correctly.
- `make test` and the race tests pass. Benchmark `BenchmarkJournalIndexedInsertWithPending` shows no material regression.
- `make lint` passes; govulncheck is clean; `go mod tidy` is clean.
