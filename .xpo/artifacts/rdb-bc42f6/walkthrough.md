# Walkthrough: readers kept stale snapshots when another process wrote

## The bug

Two RepoDB processes on one repository (two servers, a server plus an embedded app, and so on) could not see each other's writes. After an engine's second read following its own last write, it kept serving its cached snapshot indefinitely. Its next write then failed with "RepoDB data head changed". Reproduced with two `repodb start` servers in both persistence modes.

## Why

M4.4 (864731d) made reads fast by skipping the state refresh in `session.resolveSnapshot` whenever `repo.StateSeq` was unchanged. `StateSeq` was an `atomic.Uint64` on this process's `Repository`, bumped only by this process's commits, checkpoints and publications. Other processes' writes never touched it.

The subtle trigger: `lastStateSeq` was only recorded on a read, so the first read after an own write always refreshed. The stale window opened on the second read.

The shortcut existed because the honest refresh was expensive in two places:
- `Repository.Head` spawned `git rev-parse` on every call. The journal path needs it whenever the journal is clean; with a dirty journal, `WorkingState.Current` already had a cheap `stat`-based fast path.
- `Repository.Current` (native-Git) also re-read the manifest through another git subprocess, even for an unchanged commit.

## The fix

**1. `Head` is cheap and still sees other processes (`common/repository/refstamp.go`).**
Git stores `refs/repodb/data` in one of three places: the loose ref file, `packed-refs`, or reftable's `tables.list`. Every update, by RepoDB, plain `git update-ref`, or `git pack-refs`, rewrites or replaces at least one of them through a lockfile rename. That changes the file's identity, size or mtime.

`Head` stats those three paths into a `refStamp`. If the stamp equals the one stored with the cached value, it returns the cached value; otherwise it runs `rev-parse` and stores the new value with the stamp. The stamp is taken *before* `rev-parse`, so an update racing with resolution leaves a stamp that won't match next time. The race can't produce a permanently stale value. If `stat` fails, it simply always resolves.

**2. `resolveSnapshot` always checks.** The `StateSeq` comparison is gone:
- Journal mode calls `WorkingState.Current`: a journal `stat`, plus `Head` when the journal is clean.
- Native-Git mode calls `Head` and reuses the cached snapshot when the commit is unchanged, instead of calling `Current`.

The existing commit/generation comparison still decides when to re-validate.

**3. `StateSeq` removed.** The exported field, its six increments and `database.lastStateSeq` are deleted. It was only a correct signal within one process, and leaving it would invite the same mistake.

## Why a stat stamp and not a shared counter file

A RepoDB-maintained sequence file would need every writer to bump it after each change. It would miss ref changes made with plain Git, and a crash between the change and the bump would leave readers stale until the next write. Stat-ing the real storage observes the actual state.

The residual risk is a false "unchanged": an inode reused within one mtime tick with an identical size. That isn't practical on APFS, ext4 or NTFS. On coarse-timestamp filesystems it could delay visibility until the next ref change. This is documented on `refStamp`.

## Cost

`BenchmarkSQLAutocommitPointRead` (new; each query resolves a snapshot) went from ~130–153 µs to ~135–159 µs, +3–5%, from three `stat` calls. The existing warm-read benchmark runs inside one transaction, so it never measured this path.

## Tests

- `engine/cross_engine_test.go`, `TestReadersObserveOtherEngineWrites` (journal-dirty, journal-clean, native-git): the reader writes and reads, another engine writes, and the reader must see it and then write successfully. All three variants failed before the fix. The reader must write first, because the old shortcut only engaged after an own write.
- `TestHeadObservesExternalRefUpdate`: `git update-ref` rollback, `pack-refs --all`, then an update of the packed ref, all observed without reopening.
- `TestJournalIndexesMatchModel` now has one-engine and two-engines variants. The two-engine variant (the scenario that exposed this bug during rdb-e472aa) fails on the old code at step 8.
