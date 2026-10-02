# rdb-187370 walkthrough: per-table, content-keyed snapshot validation

## The problem

`ValidateSnapshot` (`engine/engine.go`) is the engine's integrity gate. For every table it decodes each stored row and checks that re-encoding the row's primary key reproduces the stored key. It also walks the data and index Prolly trees (`prolly.Reachable`) to prove every object is present. The result was cached in an LRU keyed by **(repository, commit, generation)**.

In journal mode every commit bumps the generation. `session.resolveSnapshot` (`engine/catalog.go`) validates whenever the current (commit, generation) differs from the engine's cached snapshot. So a session that started a transaction after *another* session's commit always missed the cache and re-validated the whole database. The old path also called `loadTable → ensureRows`, which built a full row map for every table. A single client never paid this, because its own commit updates `db.snapshot`. At 100k issues with 16 clients this cost 97% of CPU and most of a 6.2 GB RSS.

## The key observation

A journal commit doesn't change the manifest's tree roots. It keeps the base Git commit, and its row edits live in the journal overlay (`PendingEdits`), which `ValidateSnapshot` has never looked at. So the work validation does for generation N+1 is identical to the work for generation N. The generation was the wrong cache key. The table's **content** (its roots) is the right one.

## What changed

### Per-table cache (`engine/engine.go`)

`validatedTables` is an LRU of up to 1024 entries. `tableValidationKey` builds each key from:

- the repository identity
- the base commit (see below)
- `snapshotValidationVersion` (now 4)
- `SchemaRoot` and `DataRoot`
- the index roots, sorted by name so map order doesn't matter

The value is the table's reachable object list, the same data the old per-snapshot map held. `ValidateSnapshot` now loops over the manifest's tables, skips the ones that hit, validates the misses with `validateTable`, and records them. A table that DDL changed gets new roots, and so a new key, and is the only one re-validated.

The table name isn't in the key. Two tables with byte-identical schema and data share an entry, which is correct because validation depends only on content. (The first version of the test tripped over this: tables `a` and `b` held the same rows.)

`validatedTableObjects(snapshot, table)` keeps its signature: it finds the table's manifest entry and looks up its key. Its callers (`commitNativeGit`, `checkpointTypedEdits`, `copyTable` in merge) are unchanged, and they now also benefit from entries shared across generations.

### Why the base commit stays in the key

Without the commit, a checkpoint (new commit, mostly unchanged roots) or a fetched remote snapshot could reuse entries too. But some objects can exist only in the local journal's store, such as a schema written by DDL before a checkpoint. A remote snapshot that names the same hash without shipping the object would then pass validation, and integration validation exists precisely to catch that. Keeping the commit fixes the bug, since journal generations share their commit, without weakening that guarantee. The cost: a checkpoint re-validates each table once under its new commit. That's no worse than before, and it's now streamed.

### Streaming row validation

`validateTableRows` reads the schema with `loadTableMetadata`, opens the data tree, and walks it with `tree.Iterator`. It applies the same `decodeRow` / `encodeKey` / key-equality check that `ensureRows` used, then drops each row. A cache miss, including the first validation when a database opens, now needs memory proportional to tree depth instead of table size. The error strings are unchanged. `loadTable` had no remaining callers and was removed.

### Observability

`PerformanceCounters.TablesValidated` counts full table validations (cache misses on tables with data). The regression test asserts on it.

## Test

`TestOtherEngineCommitSkipsValidationOfUnchangedTables` (`engine/cross_engine_test.go`) opens two journal engines on one repository. The writer commits five inserts, and after each one the reader queries. The test expects `TablesValidated == 0`, where the old generation-keyed cache gives 10. After `CREATE INDEX` on one table it expects exactly 1. The engine tests don't run in parallel, so the process-wide counters are safe to assert on.

## Results (Docker, journal, `mutable` group)

With 16 clients at 100k, `issue_mixed_concurrent` went from 14 ops/s (p95 1.6 s) to 911 ops/s (p95 48 ms). Peak RSS went from 6.2 GB to 2.05 GB. The workload's CPU fell from 87.9 s to 1.95 s, and transaction start is now 0.02 s of that.

The "within ~2× of 1k" target wasn't reached: 911 vs 4,311 ops/s. The remaining gap also shows with one client, so it isn't a concurrency effect. 86% of the remaining CPU is the assignee query doing one full `Tree.Get` per matching index row, which is filed as rdb-4d8ccb. The rest of the 100k heap is in `Checkpoint`: snapshot load reads every object (rdb-93103f), and journal compaction decodes the whole journal.
