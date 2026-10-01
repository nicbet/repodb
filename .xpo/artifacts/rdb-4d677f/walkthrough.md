# rdb-4d677f walkthrough: the journal is compacted at every checkpoint

## Why

The journal (`<common-dir>/repodb/working/v1/journal`) was append-only forever. Since the lazy-replay fix (rdb-92b9e2), replay starts from the last checkpoint, so everything before it was dead weight on disk. Journal persistence is the library default, and rdb-c56142 makes it the CLI/server default, so its size must be bounded. This issue is the minimal slice of rdb-515fae. Retention policies, archiving and `repodb compact` stay there.

## How compaction works (`common/repository/journal_compact.go`)

`Checkpoint` and `CheckpointPrepared` publish the Git commit and append the `checkpoint` record, as before. Then, still holding `working.lock`, `compactAfterCheckpoint` runs `compactLocked`:

1. Read the current journal and build the **anchor**: a copy of the new `checkpoint` record plus
   - `CheckpointedTxIDs`, every transaction committed in the file being replaced;
   - `FromGeneration`, the replaced file's own anchor generation, or 0 if it still held full history.
2. Write the anchor as the only frame of `journal.tmp`, fsync it, rename it over `journal` (`robustio.Rename`), and fsync the directory (`syncDir`, a no-op on Windows).
3. Point this instance's caches at the new file.

**Crash safety.** Before the rename, the old journal (long but valid) is in place; after it, the new one is. A stray `journal.tmp` is never read and is overwritten next time.

**Best effort.** The checkpoint is already published and recorded, so a compaction error must not fail it; callers would retry work that succeeded. On error the temp file is removed, the cache is cleared (the next load re-reads the disk), `JournalCompactionFailures` is incremented, and the next checkpoint compacts the whole file. `BeforeJournalCompactionRename` and `AfterJournalCompactionRename` let tests inject faults.

## Replaying a compacted journal (`load`)

Replay used to start at generation 0, so a journal starting with a `checkpoint` record would have been "corrupt: checkpoint generation differs". A full replay whose **first frame** is a checkpoint now adopts its generation. Generations have to survive compaction, because conflict detection between engines compares `base.Generation()` with the journal's. A mismatched checkpoint anywhere else is still corruption.

## Other engines: telling journal files apart

Engines cache the journal by file identity plus a verified offset, and read only new frames. Compaction now replaces the file at every checkpoint, and filesystems like ext4 reuse inode numbers. A cached engine could then match a *different* file with the same inode that has grown past its offset, and read from the middle of it. Both caches (`workingCache` and the publication guard's `journalDirtyScan`) therefore also keep the file's **first frame header** (magic, length, CRC). `readJournalFrames(path, start, expectFirst)` reads those 12 bytes through the file it opens anyway; a mismatch returns `errJournalReplaced`, and the caller replays from the start. Every anchor is unique (new commit and generation), so its CRC identifies the file. Commit-path cache updates carry the header too (`firstHeaderAfterAppend`). Without it, every load after a commit looked like a replaced file; the existing cache-metrics test caught that during development.

## `RecoverTransaction` after compaction

Before, "transaction ID not in the journal" meant `rejected`, which is only true while the full history exists. Compaction would have turned it into a wrong answer for committed-then-checkpointed transactions. Now:
- Transaction IDs are `<generation>-<random>` (`newTransactionID`). They're internal to the journal API.
- `RecoverTransaction` looks in the current journal first, then in the anchor's `CheckpointedTxIDs`, then at the generation in the ID:
  - above `FromGeneration`: `rejected`, exact, because the anchor and the journal cover every generation since;
  - otherwise, or if the ID can't be parsed: `unknown` with `ErrWorkingHistoryTruncated`.

Answers are exact for the current and previous checkpoint interval, which is when anyone settles an uncertain commit, and honest beyond that. The anchor's ID list is bounded by one interval's transaction count, which that interval's records already held.

## Tests

- `engine/journal_compaction_test.go`:
  - bounded size over 20 checkpoints × 50 inserts (one frame each time, flat size, generations continue after reopen);
  - a second engine following compactions without false conflicts;
  - inode reuse, simulated with a hard link (without the header check it fails with `invalid frame magic`);
  - faults before and after the rename;
  - `RecoverTransaction` across compactions.
- `common/repository/journal_compact_internal_test.go`: prepared-without-commit, and the generation ranges against a hand-built anchor.
- The race detector passes on the journal concurrency tests. Checkpoints went from 180–190 ms to 185–201 ms (one small fsync plus a directory fsync).

## Found along the way

`BenchmarkJournalCheckpoint/batch=10` took about 1.1 s both before and after this change. Investigated at the user's request: Prolly chunk boundaries aren't content-local, so small edits rewrite up to half the tree. Filed as rdb-fb3d12.

## Process note

`start` auto-moved the parent rdb-515fae to DOING; it was moved back to BACKLOG, as agreed.
