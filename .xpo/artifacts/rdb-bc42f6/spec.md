# Readers keep stale snapshots when another process writes

## What

An engine that has resolved a snapshot keeps serving it after another engine or process commits (journal or native-Git), until the reader writes itself, and that write then fails with "RepoDB data head changed". Reproduced with two `repodb start` servers on one repository: server A reads, server C inserts, and A keeps returning the old rows while its next insert fails.

The exact trigger: `lastStateSeq` was recorded only when a read resolved a snapshot. So the stale window begins with an engine's *second* read after its own last write, and lasts until it writes again. The first read after a write always refreshed, which is why simple write-then-read sequences looked fine.

## Why it happens

`session.resolveSnapshot` returns the cached snapshot when `repo.StateSeq` is unchanged. `StateSeq` is an in-memory counter bumped only by this process's own writes (864731d, M4.4). It is a per-process signal used as if it were a global one.

The shortcut exists because a full refresh is expensive in two places:
- `Repository.Head` runs `git rev-parse` (a subprocess) on every call. `WorkingState.Current` needs it when the journal is clean. With a dirty journal it already has a cheap, cross-process-correct fast path: `stat` the journal and compare identity and size.
- `Repository.Current` (native-Git) additionally re-reads the manifest through another git subprocess on every call, even when the commit is unchanged.

## How

1. **Cheap, cross-process-correct `Head`** (`common/repository/refstamp.go`). `Repository` caches the resolved data-ref value together with a *ref storage stamp*: `os.Stat` of `<common-dir>/refs/repodb/data`, `<common-dir>/packed-refs` and `<common-dir>/reftable/tables.list`, recording presence, `os.SameFile` identity, size and mtime. If the stamp is unchanged, `Head` returns the cached value without a subprocess; otherwise it runs `rev-parse` and re-stamps.

   The stamp is taken *before* `rev-parse`, so a concurrent update during resolution changes the stamp, and the next call re-resolves. A stat error falls back to always resolving. `Repository.Current` uses `Head`.
2. **Drop the per-process shortcut.** `resolveSnapshot` no longer consults `StateSeq`:
   - Journal mode calls `WorkingState.Current`, now cheap in both clean and dirty states.
   - Native-Git mode calls `Head` and reuses the cached snapshot when the commit is unchanged; otherwise it loads `SnapshotCommit(head)`.

   The existing commit/generation comparison still decides when to re-validate. The exported `Repository.StateSeq` field, its six increments and `database.lastStateSeq` are removed. Keeping the field would invite the same mistake, and nothing outside the engine used it.
3. **Tests.**
   - `engine/cross_engine_test.go`, `TestReadersObserveOtherEngineWrites`: journal-dirty, journal-clean and native-Git. The reader writes and reads, another engine writes, then the reader must see the rows immediately and write successfully. All three fail before the fix.
   - `TestHeadObservesExternalRefUpdate`: a raw `git update-ref` rollback, then `pack-refs --all`, then an update of the packed ref, each observed by `Head`.
   - `TestJournalIndexesMatchModel` is split into one-engine and two-engines variants. The two-engine variant fails before the fix at step 8.
4. **Benchmark.** New `BenchmarkSQLAutocommitPointRead` (native-git, journal-clean, journal-dirty).

## Decisions

- **Stat stamp over a RepoDB-written sequence file.** A shared counter file would need every writer to bump it after each change, would miss ref changes made outside RepoDB, and would leave readers stale forever if a writer crashed between the change and the bump. Stat-ing the real storage observes the actual state.
- **Residual risk accepted.** A false "unchanged" would need an inode reused within one mtime tick with an identical size. That is not practical on APFS/ext4/NTFS; on coarse-timestamp filesystems it could delay visibility until the next ref change. Documented on `refStamp`.
- **No TTL/periodic forced refresh.** It would add latency spikes for a case that the stamp already covers.
- **Measured cost accepted.** An autocommit point read went from ~130–153 µs to ~135–159 µs (+3–5%, three extra `stat` calls), the price of correctness.

## Acceptance criteria

- The two-engine repro shows the other engine's rows immediately in journal (clean and dirty) and native-Git modes, both in engine tests and with two real server processes.
- A raw `git update-ref` of `refs/repodb/data`, including after `pack-refs`, is observed by `Head` without a restart.
- The two-engine model test passes.
- The autocommit point-read benchmark shows no material regression (+3–5%).
- `make test`, race tests and `make lint` pass.
