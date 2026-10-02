# Per-key write conflict detection with commit rebase (journal)

## What

`WorkingState.CommitTypedEdits` stops rejecting a transaction just because the journal generation moved. Under `working.lock` it compares the transaction's writes with the writes committed since its base. When they are disjoint, it commits on top of the current state: first-committer-wins snapshot isolation. Native-git mode and the untyped `WorkingState.Commit` keep the strict check.

## Why

In `mixed_80read_20write` each client updates its own `bench` row (`id = 1 + w`), yet 4% (journal) of attempts are rejected at 50k rows, because any commit since a transaction's snapshot rejects it.

## Key observation: the data rebase is already free

A journal transaction appends only its own row edits (rdb-a6a4d2), and `applyTypedEditsToSnapshot` applies them to `view.snapshot`, the **current** state, not the transaction's base. Once the generation check is relaxed, committing a disjoint transaction is the rebase. The new work is (1) deciding what "disjoint" means and (2) keeping the engine's per-generation index-edit cache correct.

## Design

### 1. Write set of a transaction (repository, `TypedTableEdit`)

- **Row keys:** `Edits[].Key` (puts and deletes). An update that changes the primary key already emits a delete of the old key and a put of the new one.
- **Unique-index claims (new):** `TypedTableEdit.Claims []IndexClaim{Index string, Key []byte}`, JSON `claims,omitempty`. The engine fills it with every **put** this transaction made to a `UNIQUE` secondary index, which is `overlay.local` of each unique index. A non-NULL unique index key is the column values without the primary key, so two transactions that give different rows the same unique value claim the same key. Keys with a NULL have the primary key appended and never collide. Deletes need no claim: a transaction can only free a value that its base snapshot held, and no concurrent transaction could have claimed that value against a snapshot where it was taken.
- **Table-level change:** `Schema != nil` (new table or schema change) or `Drop`.

Claims are journaled because another process's commits must also be checked: the journal is the only shared state. The field is additive and `omitempty`, so `workingFormatVersion` stays 3. Under alpha, no migration (Nate has no real repos yet).

### 2. Write log (repository, `workingView`)

`workingView` gets a `writes *writeLog`, holding the last generation at which each key was written, since the current base commit:

```go
type writeLog struct {
	from   uint64                       // generations <= from are not covered
	tables map[string]uint64            // last schema change or drop
	rows   map[string]map[string]uint64 // table -> row key -> generation
	claims map[string]map[string]uint64 // table -> index+"\x00"+key -> generation
}
```

- It is updated where typed edits are applied: in `materialize` (replay, full or incremental, so other processes' commits are covered) and after a successful append in `CommitTypedEdits`.
- It is reset (`from = generation`) on a checkpoint, a base change, `reconcileHead`, and an untyped `prepare` commit, which replaces the whole manifest.
- Memory is one map entry per distinct key since the last checkpoint, the same order as the pending edits already held.
- Only `working.lock` holders read or write it. Entries are only ever added with increasing generations, so a stale extra entry can only cause a spurious conflict, never a missed one.

### 3. Commit check (`CommitTypedEdits`, under the lock, after `load`)

1. `base.Commit != view.snapshot.Commit` → `ErrConflict`. A checkpoint since the base still rejects. Rebasing across a checkpoint is out of scope.
2. `base.Generation() == view.generation` → commit as today.
3. `base.Generation() < writes.from` → `ErrConflict`, because the log does not cover the gap.
4. For each `TypedTableEdit` `te`, where `g > base.Generation()` means "written since the base":
   - `te.Drop` or `te.Schema != nil`, and the table has any entry (table, row or claim) with `g > base` → conflict.
   - `writes.tables[te.Table] > base` → conflict, because someone changed or dropped the schema this transaction was built on.
   - any `te.Edits[].Key` with `rows[table][key] > base` → conflict.
   - any claim with `claims[table][index\x00key] > base` → conflict.
5. Otherwise append and apply exactly as today. The record shape is unchanged (`typed-prepare` + `commit`, `Generation = view.generation + 1`), so crash recovery, `RecoverTransaction`, compaction and replay need no changes.

A conflict returns `fmt.Errorf("%w: table %s: …", ErrConflict, …)` naming the table and kind (row, unique index `name`, schema). `errors.Is(err, ErrConflict)` still holds.

### 4. Engine (`engine/catalog.go`)

- `commitTypedEdits` fills `te.Claims` from the local puts of unique indexes in `state.idxEdits`.
- **Index-edit cache after a rebase.** `storeIndexEdits` today caches `tx.idxEdits` as the index state of the new generation. After a rebase those edits miss the intervening commits. A commit is rebased when `next.Generation() != base.Generation()+1`. In that case:
  - if `d.indexEdits` is cached for `next.Generation()-1` on the same commit, then for each table: a clean table carries over; a dirty table with index edits gets `cached[table][index].With(overlay.local)`; a dirty table without a cached entry is dropped from the cache.
  - otherwise the cache is left alone (a miss later, never wrong).

  This replaces the issue's "re-derive index edits against the new base": re-deriving costs one base-tree read per pending edit (630 ms at 10k pending, rdb-e472aa) and would undo that fix. The transaction's local index edits are deletes of its rows' old index keys and puts of their new ones. Because its rows are disjoint from every intervening write, the old values it saw are still the current ones, so its local edits are exactly right on top of the current generation.
- Schema-changing and dropping transactions conflict whenever the table was touched, so a rebased commit never needs fresh schema metadata. The metadata cache stays keyed by commit and generation.

### 5. Guarantee and write skew

This is snapshot isolation: reads are not tracked. Two transactions that each read a condition and write **different** rows can both commit (write skew). Examples: each takes one of two "on-call" doctors off duty after checking that the other is on; or a range `DELETE` races a concurrent insert into that range (a phantom). To serialize such transactions, have both write a common row, such as a guard or counter row, which turns the race into a key conflict. Lost updates (`UPDATE … SET v = v + 1` on the same row) still conflict.

## Docs

- `docs/sql.md` (transactions): replace the repository-wide conflict rule with per-key rules, the unique-index rule, the schema and checkpoint rules, and the write-skew note with the guard-row workaround.
- `docs/library.md` §Conflicts: the same, briefly.
- `docs/architecture.md`: §Commit (the base check becomes the write-log check, plus `claims` in the record) and the "optimistic and repository-wide" paragraph (journal is per-key; native-git stays repository-wide). Remaining rdb-df092b items (group commit, automatic retry) stay cited.
- `docs/benchmarks/latest.md` is not edited; it is replaced only when a scorecard is published.

## Acceptance criteria

- [ ] Journal: two transactions inserting/updating disjoint rows both commit; both rows are visible afterwards; the journal replays to the same state in a fresh process.
- [ ] Journal: same-row writers still conflict (update/update, insert/insert of the same PK, delete/update).
- [ ] Journal: disjoint rows inserting the same non-NULL unique value → the second `COMMIT` fails with `ErrConflict`; NULLs in that unique column do not conflict.
- [ ] After a rebased commit, secondary-index lookups and a later unique check see both transactions' rows (cache correctness).
- [ ] Schema change, `CREATE TABLE` of the same name, or `DROP TABLE` on either side of a touched table → conflict; DDL on an untouched table does not block a disjoint row writer.
- [ ] Cross-process (two engines, one repo): disjoint commits rebase; overlapping ones conflict.
- [ ] A checkpoint since the base → conflict.
- [ ] Crash: fault injection around a rebased commit (`BeforeJournalAppend`, `AfterJournalFlush`, a torn tail) recovers like any journal commit.
- [ ] Native-git: disjoint writers still conflict (existing tests keep their expectation for that mode); journal-mode tests that expected a conflict for disjoint rows are updated.
- [ ] `dbbench` concurrency: `mixed_80read_20write` reports 0% rejections in journal mode, and `contended_increment` still passes its lost-update check.
- [ ] `go test ./...` passes; docs updated as above.

## Out of scope

- Native-git mode; rebasing across a checkpoint; read-set tracking (serializability); automatic retry; group commit (other rdb-df092b children).
