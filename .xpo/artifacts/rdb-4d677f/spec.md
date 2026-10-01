# rdb-4d677f: Truncate the journal at checkpoint

## What

`<common-dir>/repodb/working/v1/journal` grows forever. After a checkpoint, every record before the new `checkpoint` marker is dead weight: replay starts from the last checkpoint's base (rdb-92b9e2). Rewrite the journal down to that marker at each checkpoint, so its size depends on the work since the last checkpoint, not on the repository's age.

## How

### Compaction step (`common/repository/working.go`)

After `Checkpoint` / `CheckpointPrepared` have published the commit and appended the `checkpoint` record, still holding `working.lock`:
1. Write `journal.tmp` in the same directory, holding one frame: the **anchor**, a `checkpoint` record (same `Generation`, `BaseCommit`, `GitCommit`) plus the recovery fields below.
2. `fsync` the temp file, rename it over `journal` (`robustio.Rename` for Windows sharing violations), then `fsync` the directory.
3. Update this instance's caches (`w.cache` view and offset, the dirty-scan cache) to the new file.

A crash at any point leaves either the old journal, valid and simply long, or the new one. A stray `journal.tmp` is ignored by readers and overwritten (`O_TRUNC`) by the next compaction.

### Replay of a compacted journal

A full replay that finds a `checkpoint` record as its **first frame** adopts that record's generation and base (`view.generation = record.Generation`). Before this change, replay started at generation 0 and would report "checkpoint generation differs". Generations must survive compaction: transaction conflict detection compares `base.Generation()` with the journal's. A `checkpoint` whose generation differs from the replayed one anywhere else is still corruption.

### Detecting a replaced journal (other engines and processes)

Caches identify the journal by `os.SameFile` plus "size ≥ cached offset". Once journals are replaced routinely, inode numbers can be **reused** (common on ext4). A cache could then match a different file that has grown past its offset, and read from the middle of it. Both caches (`workingCache` and the publication guard's `journalDirtyScan`) therefore also remember the journal's **first frame header** (12 bytes: magic, length, CRC) and re-check it on each incremental read, through the file already being opened. A mismatch forces a full replay. Every anchor differs (new `GitCommit` and `Generation`), so its CRC identifies the file.

### `RecoverTransaction` after compaction

It must never report a committed transaction as rejected. Before this change, "transaction ID not found" meant `rejected`, which is exact only while the whole history is present.
- **Transaction IDs carry their generation:** `"<generation>-<32 hex>"` instead of 32 hex. They're internal; only `WorkingCommitError.TransactionID` and `RecoverTransaction` see them.
- **The anchor records** `CheckpointedTxIDs` (every transaction committed in the file it replaced) and `FromGeneration` (the replaced file's own anchor generation, or 0 if it held full history).
- **Lookup:**
  1. found in the current journal: committed if it has its `commit` record, otherwise rejected (as before);
  2. found in the anchor's `CheckpointedTxIDs`: committed;
  3. otherwise, with generation `g` parsed from the ID:
     - `g > FromGeneration`: rejected (exact: the anchor and the current journal cover every generation since);
     - `g ≤ FromGeneration`, or an unparseable ID: `OutcomeUnknown` with `ErrWorkingHistoryTruncated` ("transaction is older than the retained journal history").

This answers exactly for the current and the previous checkpoint interval, which covers any realistic recovery call, made right after an uncertain commit. It is honest, not wrong, beyond that. The anchor's ID list is bounded by one checkpoint interval's transaction count, which that interval's journal records already held.

### Failure policy

Compaction runs after the checkpoint is published and recorded, so failing it must not fail the checkpoint. On error the temp file is removed, the old journal stays valid, and the next checkpoint compacts everything. Metrics `JournalCompactions` and `JournalCompactionFailures` make repeated failures visible. Fault points `BeforeJournalCompactionRename` and `AfterJournalCompactionRename` support tests.

## Decisions (confirmed by the user, 2026-10-01)

1. **Best-effort compaction** (above). The alternative, returning an error from a published checkpoint, would make callers retry work that already succeeded.
2. **Generation-tagged transaction IDs plus one interval of IDs in the anchor**, for exact `RecoverTransaction` answers without keeping old records. Alternatives considered:
   - keep the previous interval's full records: exact too, but up to 2× the disk, since typed edits are large;
   - answer `unknown` for any ID missing after a compaction: simpler, but it degrades the common "never reached disk → rejected" answer after every checkpoint.
3. **No journal format version bump.** New code reads uncompacted journals unchanged, and alpha needs no compatibility with older binaries.

## Tests

- **Bounded size:** 20 checkpoints, each after 50 inserts. After each, the journal holds exactly one frame (the anchor), and file size doesn't grow with N.
- **Second engine:** engine B reads, writes and checkpoints across A's compactions. Its generation stays consistent, there are no false `ErrConflict`s, and rows are correct.
- **Inode reuse:** a cache whose `SameFile` matches but whose first-frame header differs. The load does a full replay and returns correct data.
- **Faults:** errors injected before and after the rename leave a loadable journal with every acknowledged transaction. The checkpoint still succeeds, the failure metric counts it, and the next checkpoint compacts.
- **`RecoverTransaction`:** committed before compaction → committed; committed two intervals back → unknown with `ErrWorkingHistoryTruncated`; never written (current generation) → rejected; prepare without commit → rejected.
- **Existing journal tests** (replay, stale writer, recovery, checksum corruption, bounded snapshot loads) keep passing.

## Docs

- `docs/architecture.md`: journal growth (now bounded; rdb-515fae keeps retention and `repodb compact`), the anchor record, and replaced-file detection.
- `docs/cli.md`: backup notes (the journal holds only work since the last checkpoint).
- `docs/library.md`: `RecoverTransaction`'s answers after a checkpoint.

## Acceptance

As listed in the issue, plus the tests above. `make test` and `make lint` pass.

## Implementation notes (as built)

- **Code.**
  - `common/repository/journal_compact.go`: `encodeFrame`, `compactLocked`, `compactAfterCheckpoint`, `newTransactionID`/`transactionGeneration`, `recoverFromAnchor` and `ErrWorkingHistoryTruncated`.
  - `syncdir_other.go` / `syncdir_windows.go`: directory fsync, a no-op on Windows, where directory handles can't be flushed.
  - `working.go`: the anchor fields on `journalRecord`, first-header identity in both caches, `readJournalFrames(path, start, expectFirst)`, anchor generation adoption in `load`, `RecoverTransaction` falling back to the anchor, and new metrics and fault points.
- **The checkpoint's own cache.** `Checkpoint`/`CheckpointPrepared` previously pointed `w.cache` at the appended journal. Now `compactAfterCheckpoint` points it at the compacted file, or clears it on failure, so the next load re-reads whatever is on disk.
- **Commit cache entries.** These record the first header too (`firstHeaderAfterAppend`). Without it, every load after a commit looked like a replaced file and did a full replay; the existing cache-metrics test caught this.
- **Second engine.** Covered by separate engines in one process. Each engine has its own `Repository`, `WorkingState` and file descriptors, which is what the cache logic depends on. No subprocess test.
- **Inode reuse.** Simulated with a hard link that keeps the old inode, then moving the longer new journal's bytes into it. Without the header check the test fails with `invalid frame magic`: the cached engine reads from the middle of the other file.
- **Prepared without commit.** Covered by an internal test (`journal_compact_internal_test.go`) with a hand-built anchor and journal, which also checks the generation ranges of `recoverFromAnchor`.
- **Cost.** `BenchmarkJournalCheckpoint` (1x, 5 runs) went from 180–190 ms to 185–201 ms: one small fsync plus a directory fsync. `batch=10` takes ~1.1 s both before and after; that's pre-existing and unrelated to compaction.
- No existing test needed adjusting.
