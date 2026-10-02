# Spec: fewer Git subprocesses per sync

## Baseline (measured 2026-10-02 at ab20fa6)

`repodb sync` in journal mode, 50k rows in 1 table (408 objects), traced with `GIT_TRACE2_EVENT`. "Top-level" counts only processes RepoDB starts itself. Children such as `upload-pack`, `pack-objects` and `maintenance` are excluded.

| scenario | wall | top-level git procs | all git procs | full snapshot loads |
|---|---|---|---|---|
| up-to-date | 387 ms | 26 | 30 | 4, all of the same commit |
| push (`--commit`) | 802 ms | ~55 | 65 | 6 |
| fast-forward pull | 431 ms | ~28 | 32 | 2 |
| merge (`--commit`) | 1084 ms | ~62 | 88 | 9; local head loaded 5× |
| conflict | 867 ms | ~55 | 66 | 9 |

Git accounts for 25–45% of wall time. The rest is in-process work, mostly re-hashing every object on each repeated load.

Each full load is `show <c>:manifest.json`, then `ls-tree -r`, then a one-shot `cat-file --batch` over **every** object, then a SHA-256 check of every object. Where the redundancy comes from:

- **Repeated loads of one commit.** The same commit is loaded by `OpenWithSnapshot`, `WorkingState` replay/status (`requireCleanWorkingState` runs twice per attempt), the checkpoint engine's open, and `SnapshotAt` in sync. Each runs on its own `Repository` instance, so nothing is shared.
- **`Discover`** runs 3 `rev-parse` processes and is called 3× per sync (9 procs).
- **`SnapshotAt(commit)`** with an already-resolved commit costs an extra `rev-parse --verify`.
- **Ancestry checks** take `merge-base --is-ancestor` ×2 plus `merge-base --all`.
- **`fetch()`** runs `ls-remote`, then `fetch`, then `rev-parse`. It runs even when the remote ref already equals the tracking ref, and again after every successful push only to learn what we just pushed. Every `fetch` also triggers `maintenance --auto`.
- **`snapshotStore.Get` cache misses** start one `git show` per object.

## Scope

This issue: subprocess count and redundant loads of the same commit. **Not** in scope: the O(database) cost of a single load (reading and hashing every object). That is rdb-93103f, which builds on the long-lived reader added here.

## Design

### 1. Process-wide weak memo of validated snapshots (`common/repository`)
- `loadSnapshot` consults a process-wide map `(Repository.Identity(), commit) → weak.Pointer[snapshotCore]`. Only weak references are held, so the memo never keeps a snapshot alive by itself: a commit is reused only while some caller (engine, working state, sync) still holds a snapshot of it.
- The shared core holds the immutable parts: manifest, object set, OIDs and the `snapshotObjectCache`. Each `Snapshot` returned (hit or miss) is a fresh struct that references the core, with `generation = 0` and `pendingEdits = nil`. Callers mutate `generation` (working.go), so a shared `*Snapshot` is never handed out.
- `Writer.Commit` registers the snapshot it just verified, so the post-checkpoint `SnapshotCommit(result.Commit)` is a hit while the result is held.
- Cleared entries are swept on insert (`weak.Pointer.Value() == nil`), or through `runtime.AddCleanup`. Errors are never cached.
- Tests get an unexported reset hook.
- Rationale: commit IDs name immutable content, and the first load verified every object. Re-verification in the same process would only catch corruption after validation, which a running engine already does not re-check. A strong LRU was rejected: until rdb-93103f, a snapshot holds every object's bytes (~350 MB at 100k issues).

### 2. Long-lived `cat-file --batch` per repository (`common/git`)
- A registry keyed by worktree root (the directory Git runs in; `Repository.Root`). It starts `git cat-file --batch` lazily and serializes requests with a mutex. It parses `<oid> <type> <size>` and `missing`, and accepts `<rev>:<path>` requests.
- Used for `manifest.json` (replaces `show`), the object reads in `loadSnapshot`, and `snapshotStore.Get` misses (replaces one `git show` per object).
- Requests are pipelined: write all IDs, then read the replies, with a writer goroutine so large batches can't deadlock on pipe buffers.
- Failure handling: any I/O or protocol error, or a cancelled `ctx` mid-request, kills the process. The next request restarts it, so errors are never sticky.
- Lifecycle has two parts:
  - **Idle close after 5 s** is the safety net, so short CLI runs and ad-hoc `Repository` users don't accumulate children.
  - **`git.CloseReaders(root)`** closes deterministically. `engine.Close` calls it, and so do tests that need it before temp-dir cleanup. Windows needs this: an open `cat-file` holds pack files open.
  - When the parent exits, the child gets EOF on stdin and exits.
- New objects: `cat-file --batch` finds new loose objects, and re-scans packs when an object is missing (rdb-2124bc relies on this). That covers objects written or fetched after the reader started.
- `ProcessCount` counts a reader start as one process.

### 3. Fewer `rev-parse` calls
- `CLI.Discover`: one `rev-parse --show-toplevel --path-format=absolute --git-common-dir --show-object-format` (3 → 1).
- Sync passes already-resolved commits to `SnapshotCommit`, not `SnapshotAt`.

### 4. One ancestry query
- Sync runs `merge-base --all local remote` once:
  - base == remote → push;
  - base == local → fast-forward;
  - otherwise merge, using the sorted-first base as today.
- This replaces two `--is-ancestor` calls and the later `MergeBase`.

### 5. Cheaper `fetch()`
- `ls-remote` as today. If the remote ref is missing → `(_, false)`.
- If the remote OID equals the tracking ref and the commit exists locally (checked through the long-lived reader, no new process), **skip `git fetch`**. This also avoids `upload-pack` and `maintenance --auto`. A missing object still fetches, so a damaged clone is repaired as before.
- Otherwise `fetch`, then resolve the tracking ref.
- After a successful `PushCommit(C)`, report `C` as the remote head instead of running `ls-remote` + `fetch` + `rev-parse`.
  - Verified: `git push <commit>:refs/repodb/data` already updates the tracking ref through the fetch refspec that `enable` configures, so no `update-ref` is needed.
  - If the remote moves after the push, the next sync sees it.

## Acceptance criteria
- [ ] Rerunning the probe (`scratchpad/probe.sh`, 50k rows), top-level git processes are at most: up-to-date **12**, fast-forward **17**, merge **35**. The fast-forward target was 16 at spec time; the extra process is `fetch()`'s pre-check `rev-parse`, kept rather than resolving refs through the long-lived reader. Each distinct commit is fully loaded at most once per process.
- [ ] `BenchmarkSyncUpToDateWarm` and `BenchmarkSyncDivergent` `git-procs/op` drop. Before/after numbers go in the completion comment.
- [ ] Memo tests:
  - copies are independent: mutating `generation` on one doesn't affect another;
  - a held snapshot is reused, so a second load starts no git process;
  - once every holder is dropped and GC runs, the entry is gone.
- [ ] Reader tests:
  - the reader survives a missing object;
  - it restarts after being killed and after ctx cancellation mid-read;
  - it sees objects written after it started;
  - it closes when idle, and `CloseReaders` closes it immediately.
- [ ] Sync tests still pass, including retry, conflict, rejected-push and the fault-injection tests. `make test` and `make lint` pass, and the Windows cross-build passes.
- [ ] `docs/architecture.md` describes the long-lived reader and the snapshot memo (and `docs/cli.md` if sync's tracking-ref behavior is documented there).

## Decisions (confirmed by the user, 2026-10-02)
1. Memo trust: accepted. A **weak** memo instead of a 4-entry LRU, so it adds no memory retention.
2. Skip `fetch` when `ls-remote` matches the tracking ref (with a local-presence check), and set the tracking ref locally after a push: both accepted.
3. Reader lifecycle: an idle timeout plus explicit `git.CloseReaders`, called from `engine.Close` and tests.
4. `OpenWithSnapshot` resolves the head through `Head()` (primes the head cache), which saves one `rev-parse` per open. This was added during implementation.
