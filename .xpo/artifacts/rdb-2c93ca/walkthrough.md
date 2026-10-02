# Walkthrough: fewer Git subprocesses per sync

## The problem, measured

A `GIT_TRACE2_EVENT` probe of `repodb sync` (journal mode, 50k rows) counted 26 top-level Git processes for an up-to-date sync and 72 for a merge. Most came from **loading the same commit again and again**. `OpenWithSnapshot`, journal replay/status (run twice per attempt), the checkpoint engine's open and sync's own `SnapshotAt` each have their own `Repository`, and nothing was shared between them. Every load was `show` + `ls-tree` + a one-shot `cat-file --batch` over every object, plus a SHA-256 of every object.

The rest came from small inefficiencies:
- 3 `rev-parse` calls per `Discover`;
- `rev-parse --verify` on commits that were already resolved;
- three ancestry calls;
- `ls-remote` + `fetch` + `rev-parse` even when nothing had changed, and again after every push.

## The pieces

### 1. Long-lived object reader (`common/git/reader.go`)

`objectReader` keeps one `git cat-file --batch` per worktree root, in a process-wide registry.
- **Callers:** `ReadObjects`, `ReadTreeFile` (the manifest, and `snapshotStore.Get` misses, which used to start one `git show` per object) and the new `HasCommit` all go through it.
- **Serialization:** `read` holds the reader's mutex for a whole request batch.
- **No pipe deadlock:** requests are written from a goroutine while replies are read, because cat-file answers as it reads and a large batch would otherwise fill both pipes.
- **Errors are never sticky:** any protocol or I/O error, or a cancelled context (`context.AfterFunc` kills the process), discards the process. The next request starts a new one. If a *reused* idle process turns out to be dead, the request is retried once on a fresh one; reads are idempotent.
- **Lifecycle:**
  - an idle timer (5 s) closes the process, so CLI runs and ad-hoc `Repository` users leave nothing behind;
  - `CloseReaders(root)` closes it at once, and `engine.Close` calls it. On Windows an open cat-file holds pack files open, which would block `git gc` and temp-dir removal.
- **Freshness:** Git finds new loose objects and re-scans packs on a miss, so the reader sees objects written or fetched after it started.

### 2. Weak snapshot memo (`common/repository/snapshotmemo.go`)

`snapshotMemo` maps `(Repository.Identity(), commit)` to `weak.Pointer[snapshotCore]`.
- **The core** is the validated, immutable content: manifest, object set, OIDs and object cache. Each `Snapshot` loaded from a commit references its core, and that reference is what keeps the core alive.
- **Fresh structs on every hit:** `loadSnapshot` returns a new `Snapshot` built from the core on every hit (and on the miss path too), because callers mutate `generation` on the snapshots they receive (working.go). Handing out a shared pointer would leak working state between callers. Manifest maps are shared; I checked that every mutation site builds a fresh manifest.
- **Commits register too:** `Writer.Commit` registers its verified result, so the post-checkpoint `SnapshotCommit(result.Commit)` is free.
- **Why weak, not an LRU (decided in review):** until rdb-93103f, a snapshot holds every object's bytes (~350 MB at 100k issues). A strong cache would pin old heads. A weak memo adds no retention. Sync's repeat loads happen while an earlier holder (the engine, the journal view, `openedSnapshot`) is still alive, so they still hit.
- **The trust trade-off (accepted in review):** a commit validated earlier in the process isn't re-hashed. Corruption that appears on disk afterwards goes unnoticed until the snapshot is collected and read again, which is the same trust a running engine already places in its snapshot.

### 3. Sync (`integration/integration.go`)

- **Ancestry:** one `MergeBases` (`merge-base --all`). A side is behind exactly when its head is the *only* merge base. That replaces `IsAncestor` ×2 plus `MergeBase`. No common ancestor (exit 1) now gives the clear "no common ancestor" error.
- **Resolved commits:** these go to `SnapshotCommit`, not `SnapshotAt`.
- **`fetch()`:** after `ls-remote`, it resolves the tracking ref. If that already equals the remote head and `HasCommit` confirms the commit is local, `git fetch` is skipped, which also skips `upload-pack` and `maintenance --auto`. A missing commit still fetches, so a damaged clone gets repaired.
- **After a push:** `RemoteHead` is the pushed commit, with no re-fetch. I verified that `git push <commit>:refs/repodb/data` updates `refs/repodb/remotes/<remote>/data` itself, through the fetch refspec `enable` configures. `TestSyncTrackingRefWithoutRedundantFetches` asserts both behaviors with a trace2 log.

### 4. Smaller things

- `CLI.Discover` makes one `rev-parse` with all three flags.
- `OpenWithSnapshot` resolves through `Head()`, which primes the head cache (`refStamp`), so the caller's next `Head()` starts no process.

## Results

| sync (probe) | procs before → after |
|---|---|
| up-to-date | 26 → 10 |
| push | 51 → 24 |
| fast-forward | 26 → 17 |
| merge | 72 → 34 |
| conflict | 60 → 25 |

`BenchmarkSyncUpToDateWarm` went from 16 to 7 procs (125 → 74 ms). `BenchmarkSyncDivergent` went from 39 to 22, and conflict-resolution from 72 to 32 (619 → 327 ms).

**Not done, on purpose:** resolving refs through the long-lived reader. It would save the `fetch()` pre-check `rev-parse` (fast-forward lands at 17, not 16), but a stale ref answer there would make sync skip a fetch it needs.

## What's next

rdb-93103f (a load reads and hashes every object) can now read objects lazily through the reader at no process cost, and verify only what it touches.

## Also on this branch

rdb-f8b841: a pre-existing `make lint` failure (SA4000 in `experiments/dbbench/workloads_test.go`).
