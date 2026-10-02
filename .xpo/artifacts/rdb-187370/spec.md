# rdb-187370: Validate snapshots per table, keyed by content roots

## What

`ValidateSnapshot` (`engine/engine.go`) caches results per **snapshot**, keyed by (repository identity, commit, generation). Every journal commit creates a new generation, so the cache always misses. Each session that starts a transaction after another session has committed then re-validates every table: it decodes every row through `loadTable → ensureRows`, which builds a full row map, and runs `prolly.Reachable` over every data and index tree.

Change validation so that a table already validated in this repository is not validated again, and so that validation never builds a row map.

## Why

A journal commit keeps the base Git commit and the manifest's tree roots. Pending edits live in the journal overlay, and `ValidateSnapshot` never reads them. So what `ValidateSnapshot` checks for generation N+1 is byte-for-byte what it already checked for generation N. All the work is redundant. It costs 97% of CPU in `issue_mixed_concurrent` at 100k issues (14 ops/s, compared with 1,543 ops/s at 1k). It also accounts for most of the 6.2 GB peak RSS.

## How

### 1. A per-table validation cache keyed by content

Replace the snapshot-keyed LRU with a **table-keyed** LRU:

- key = repository identity, base commit, `SchemaRoot`, `DataRoot`, the sorted `(index name, root)` pairs, and `snapshotValidationVersion`
- value = the table's reachable objects (`[]storage.Hash`, as now)

`ValidateSnapshot` loops over `snapshot.Manifest.Tables`. For each table, it looks up the key. On a hit it skips that table. On a miss it validates the table (step 2) and stores the entry. A snapshot is valid when all of its tables are.

`validatedTableObjects(snapshot, table)` keeps its signature. It builds the key from `snapshot.Manifest.Tables[table]` and returns the cached objects. Its callers (`commitNativeGit`, `checkpointTypedEdits`, `copyTable`) don't change.

Capacity: 1024 table entries, up from 128 snapshots. Successive journal generations share entries, so the cache holds about one entry per table per base commit, not one per generation.

The commit stays in the key on purpose (see Decisions), so a checkpoint still re-validates each table once under its new commit. That is the same cost as today, but it now happens once per checkpoint, not once per transaction.

### 2. Validate rows without building a row map

Add `validateTableRows(ctx, store, manifestTable)`. It loads table metadata (`loadTableMetadata`), opens the data tree, and walks it with `tree.Iterator`. For each entry it runs `decodeRow`, `encodeKey` and the key-equality check from `ensureRows`, then discards the row. `ValidateSnapshot` calls this instead of `loadTable`. Error messages and checks stay the same, so tests that expect validation failures still pass. This keeps memory at O(tree depth) on a miss, including the one-off validation when a database opens and the validation after a checkpoint.

`loadTable` stays for its other callers (if there are none after this change, remove it).

### 3. No behaviour change elsewhere

`resolveSnapshot`, `NewWithOptions`, `Checkpoint` and the integration paths still call `ValidateSnapshot`. They now pay only for tables whose roots changed.

## Decisions

- **Commit stays in the table key.** Dropping it would also let checkpoints and fetched remote snapshots reuse unchanged tables. But a journal-only object (for example a schema written to the journal before a checkpoint) could then vouch for a remote snapshot whose Git store lacks that object. Integration validation exists to catch that case. The bug is about journal generations, which share a commit, so keeping the commit fixes it without that risk. Revisit if checkpoint validation shows up in profiles.
- **Pending edits stay unvalidated here**, as they are today. They are typed edits the engine wrote and checksummed in the journal.

## Edge cases

- DDL in journal mode (new `SchemaRoot`, or a new or dropped index root) → that table's key changes → only that table is re-validated.
- A table with no `DataRoot` (empty table): cache its schema root only, as now.
- Two sessions missing the same key at the same time may both validate it. That is harmless, the same as today.
- `snapshotValidationVersion` is bumped to 4, because the shape of the cache key changes.

## Acceptance criteria

- [ ] Starting a transaction after another session's journal commit does no row decoding and no `prolly.Reachable` when the manifest roots are unchanged. A test checks this with `performanceCounters.rowsDecoded`, or with a new validation counter, across two engines/sessions.
- [ ] DDL on one table re-validates only that table (test).
- [ ] Existing corruption tests still fail validation with the same errors. A corrupt row is still caught on first validation.
- [ ] `ValidateSnapshot` on a miss does not hold all rows of a table in memory (it uses the iterator; checked by code review).
- [ ] dbbench `mutable` group at 100k issues in Docker: `issue_mixed_concurrent` with 16 clients is within ~2× of its 1k throughput, and the heap no longer grows with table size during the concurrent phase. Record the numbers in the completion comment. `docs/benchmarks/latest.md` is not republished in this issue.
- [ ] `go test ./...` passes.

## Docs

`docs/architecture.md`: update the description of snapshot validation and caching, if it has one.
