# Walkthrough: `repodb check`

## Why this exists

rdb-93103f made snapshot opens lazy: opening reads the manifest and `ls-tree`, and each object is hashed against its name only when something first reads it. That made opens O(inventory) instead of O(database), but it means a corrupt or missing blob in data nobody reads is never reported. `repodb check` is the agreed mitigation: an explicit, complete verification you run after a restore, after a disk fault, or on a schedule.

## The layers

### 1. `common/git`: per-object read results

`ReadObjects` fails the whole batch on the first missing object, which is right for normal reads but useless for a diagnostic that must report *all* failures. `ReadObjectResults` returns one `ObjectResult` per request (data, type, or `Missing`), in request order; `ReadObjects` is now a thin wrapper over it, so normal behavior is unchanged. `RevList` (`git rev-list <rev> --`) lists the data history for `--all`, following every merge parent.

### 2. `common/repository/check.go`: uncached load and object verification

- **`loadSnapshot` was split.** `readSnapshot` does all the open-time checks (strict manifest decode, format version, sorted/unique inventory, table roots in the inventory, tree has exactly the listed entries) and touches no memo. `loadSnapshot` wraps it with the memo lookup and `rememberSnapshot`. `LoadSnapshotUncached` exposes `readSnapshot`: the snapshot gets a fresh object cache, shares nothing read earlier in the process, and is never memoized, so later ordinary opens don't inherit it either.
- **`Snapshot.VerifyObjects(ctx, skip)`** walks `Manifest.Objects` in batches of 1,024 through `ReadObjectResults`. Each object is classified as missing, not a blob, or mismatched (`storage.Sum` ≠ name); every failure becomes an `ObjectProblem`, whose `String()` gives the report line. Good bytes go into the snapshot cache, so the SQL pass that follows reads nothing from Git again. Memory for one commit is O(its objects), the same as a full SQL validation already costs. `skip` lets `--all` pass over Git object IDs already verified earlier in the run. The returned error is reserved for operational failures (Git down, context cancelled).
- `DataHistory` and `ResolveCommit` are small helpers. `DataHistory` returns `ErrNotInitialized` when there is no data head.

### 3. `engine`: a validation cache you can instantiate

Before this change, table and node validation caches were anonymous package globals with free functions. They're now one `validationCache` type with methods (`lookupTable`, `rememberTable`, `nodeCache`, `validateTable`, `walkTable`). `processValidation` is the shared instance that `ValidateSnapshot`, checkpoints, merges and sync use, so their behavior is unchanged. The refactor exists so a check can create its **own** instance:

- Nothing validated before the check started can vouch for anything: that's the guarantee, stated structurally rather than by clearing globals (which would also have slowed down any engine sharing the process).
- The check doesn't pollute the shared cache either.
- Within one `--all` run the private cache works exactly like the shared one, including the `Snapshot.Provides` rule: a table or subtree validated for commit N is reused for commit N-1 only if that commit stores the same objects under the same Git object IDs. The commit's own bytes were all hashed by `VerifyObjects` (or by an earlier commit in the run, for the same OID), so the trust chain never leaves the run.

`maxValidatedNodeLinks` stays a package variable read under each cache's node lock; the test hook `SetMaxValidatedNodeLinks` now locks `processValidation`.

### 4. `engine/check.go`: orchestration

`Check(ctx, repo, CheckOptions{Revision, All})` resolves the commits (head by default; `Revision` and `All` are mutually exclusive), then for each commit, `checkRun.commit`:

1. `LoadSnapshotUncached`. A failure here (bad manifest, tree mismatch, unsupported format) is one problem, and the commit's other checks are skipped, because there's no trustworthy inventory to check against.
2. `VerifyObjects`, skipping OIDs in the run's `verified` set; every OID that passed is added to that set, and failed ones never are.
3. Every table, in name order, through the run-local cache's `validateTable`. A failing table is one problem (the error already names the table); the others are still checked. A table broken by a bad object is therefore reported both as the object and as the table, which tells the reader which tables are affected.
4. If every table was walked, inventory objects no table reached are **warnings** (decided with the user). If a table failed, its reachable set is unknown, so no unreferenced warnings are emitted rather than false ones.

Corruption goes into `CheckReport`/`CommitCheck` (`Problems`, `Warnings`); the `error` return is only for failures to run the check (unknown revision, Git failure, cancellation). Each commit's snapshot goes out of scope before the next, so memory doesn't accumulate across `--all`.

### 5. CLI

`repodb check [--revision <rev> | --all] [--repo <path>]` opens the repository only: no engine, no journal, no locks. `printCheckReport` prints `<commit>\t<problem>`, `<commit>\twarning: …`, `ok <commit>` per clean commit, and `checked N commit(s): …`; any problem returns `check failed: P problem(s) in C commit(s)`, which `main` turns into exit status 1.

## Deliberate limits

- **The journal isn't checked.** Uncheckpointed transactions have their own frame checksums, verified on replay. Checking them would mean opening the working state, which a read-only diagnostic should avoid.
- **No `git fsck`.** We check RepoDB's content addressing on top of whatever bytes Git returns.
- **Prolly offset cache.** It's process-wide and not bypassed, which is safe: offsets for hash H were parsed from bytes that hashed to H, so they're valid for any bytes the check accepts as H.

## Tests

`engine/check_test.go` corrupts loose objects in place (rewriting the zlib-compressed file under the same name, as a failing disk would) or deletes them:
- healthy repository: head, revision, all, unknown revision;
- corruption behind warm caches: an ordinary `SnapshotCommit` + `ValidateSnapshot` still passes (the memo and cache vouch), while `Check` reports the mismatched object, the missing object, and both affected tables, and leaves the intact table alone. The tables need distinct columns: identical schemas share one schema object;
- tree/manifest mismatch: exactly one problem;
- a table whose data root is a well-named non-node blob: one problem, other table fine; an extra listed object: one warning, no problems;
- `--all` with corruption only in an old commit: head clean, old commit failed, and only 3 table validations across the run (the unchanged table is validated once).

`cmd/repodb` tests cover the uninitialized error, flag conflict, and exact output/exit shape.
