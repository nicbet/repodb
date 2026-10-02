# `repodb check`: on-demand full verification of a data commit

## What

A `repodb check [--repo <path>] [--revision <rev> | --all]` command, backed by a public `engine.Check` API. For the data head, a given revision, or every commit in the data history, it re-runs every integrity check RepoDB has, without trusting work done earlier in the process:
- the manifest and tree inventory checks done at open;
- reading every inventory object by Git object ID and checking `storage.Sum` against its name;
- full SQL validation: schemas, rows and keys, Prolly structure, and index trees.

It reports every problem it finds, rather than only the first, and exits non-zero if there are any.

## Why

Since rdb-93103f, opening a snapshot reads only the manifest and `ls-tree`. Objects are checked on first read, so a corrupt or missing blob in a region nobody reads is never reported. This command is the agreed mitigation: an explicit, complete check that you can run after a restore, a suspicious disk event, or as a periodic job.

## Acceptance criteria

1. On a healthy repository, `repodb check` prints one `ok` line for the head commit and exits 0. `--revision <rev>` checks that commit instead. `--all` checks every commit reachable from `refs/repodb/data`, including merge parents.
2. A blob whose content no longer hashes to its name is reported as `object <hash>: content does not match its name`. A blob that Git cannot read (missing from the object store) is reported as `object <hash>: missing Git object <oid>`. Both cause a non-zero exit.
3. All object failures in a commit are reported, not just the first one.
4. An inventory/tree mismatch (an unlisted entry, a missing listed entry, a bad manifest) is reported for that commit, and that commit's object and SQL checks are skipped, since there is no trustworthy inventory to check against.
5. A SQL-level failure (undecodable row, key mismatch, broken Prolly ordering or counts, unreadable schema) is reported per table with the table name. Other tables are still checked.
6. Validation caches are not consulted for work done before the check started. A snapshot already loaded (memoized) in the process is not reused. Tests prove this: warm both caches by opening an engine, corrupt an object on disk, then run `Check` in the same process. It must report the corruption.
7. `--all` reads each distinct Git object at most once per run. It reuses table and subtree validation from earlier commits in the same run only under the existing `Provides` rule. Memory is released between commits.
8. The final summary line is `checked N commit(s): no problems` or `checked N commit(s): P problem(s) in C commit(s)`. Failures exit 1 through the existing `repodb: <error>` path.
9. `docs/cli.md` documents the command. The integrity paragraph in `docs/architecture.md` points to it instead of to rdb-a8357d.

## Flow

1. **`common/git`**: add `CLI.ReadObjectResults(ctx, root, oids)`. It returns per-request results (data, or missing / wrong type) instead of failing the whole batch on the first missing object. `ReadObjects` keeps its current behavior on top of it. Also add `CLI.RevList(ctx, root, rev)` (`git rev-list <rev>`) for `--all`.
2. **`common/repository`**:
   - Split `loadSnapshot` so that `Repository.LoadSnapshotUncached(ctx, commit)` runs the full manifest/tree check and returns a snapshot with a fresh object cache. It neither reads nor writes `snapshotMemo`.
   - Add `(*Snapshot).VerifyObjects(ctx, skip func(oid string) bool) []ObjectProblem`. It reads every listed object in fixed-size batches (for example, 1,024) through `ReadObjectResults`, checks `storage.Sum`, and caches the good bytes in the snapshot cache, so SQL validation does not read them again. It returns one `ObjectProblem{Hash, OID, Kind}` per failure. `skip` lets `--all` pass over OIDs already verified in the run.
   - Add `Repository.DataHistory(ctx)`, which lists the commits reachable from `DataRef`.
3. **`engine`**:
   - Refactor `ValidateSnapshot`/`validateTable` to take a validation-cache handle. The default handle is the existing process-wide `validatedTables`/`validatedNodes`; a check run creates a private, run-local instance with the same LRU and `Provides` logic. Behavior of `ValidateSnapshot` is unchanged.
   - Add a variant that keeps going after a table fails and returns per-table errors.
   - `engine.Check(ctx, repo, CheckOptions{Revision string; All bool}) (CheckReport, error)` orchestrates the run: resolve the commits, then for each one load it uncached, verify objects, and validate SQL with the run-local cache. `CheckReport` holds per-commit results. The `error` return is reserved for operational failures, such as Git being unavailable or a bad revision, as distinct from corruption findings.
4. **`cmd/repodb`**: add a `check` case. It uses `repository.Open` and does not open an engine or the journal. It prints problem lines as `<commit>\t<problem>` and one `ok <commit>` line per clean commit, followed by the summary. It returns an error (exit 1) if there are any problems. Add `check` to the usage string.
5. **Tests**:
   - `engine` tests cover each acceptance criterion: corrupted blob, missing blob, tree mismatch, bad row in one of two tables, warm-cache bypass, and `--all` over a history with a corruption only in an old commit.
   - A `cmd/repodb` test covers the exit status and output shape.
6. **Docs**:
   - `docs/cli.md`: a new "Checking integrity" section, placed before "Backup and recovery". It covers the flags, output and exit status, notes that the journal is not checked, and suggests running the command after restoring a backup.
   - `docs/architecture.md`: update the opening-a-snapshot paragraph and the snapshot-memo paragraph ("not detected until… read again, or `repodb check`").

## Decisions

- **Orchestrator in `engine`, not `repository`.** SQL validation lives in `engine`, which already imports `repository`. The CLI stays thin, and library users get the same check.
- **Run-local validation caches, not clearing the globals.** Clearing the process-wide caches would only cost performance for a co-hosted engine, but a private instance states the guarantee directly: nothing validated before the check is trusted. It also makes `--all` efficient.
- **Cache verified bytes during the object pass.** Verified bytes go into the snapshot's cache so that SQL validation reuses them. Memory for one commit is then O(objects in that commit), the same as a full SQL validation already costs today. The alternative, verifying and discarding the bytes, would save memory but read every object twice.
- **Report and continue, not fail-fast.** A check is diagnostic; one bad blob shouldn't hide others. The exception is an inventory failure (criterion 4), which ends that commit's check.
- **Single exit code 1** for any failure, matching every other subcommand. The output distinguishes corruption from operational errors.

## Edge cases

- **HIGH: journal state is not checked.** `check` verifies committed data only. Uncheckpointed journal transactions have their own frame checksums, verified on replay. This is documented. Checking the journal's view would mean opening the working state (and its lock), which a read-only diagnostic should avoid.
- **MEDIUM: unreferenced inventory objects.** These are objects listed in the manifest that no table reaches. Writers call `RetainOnly`, so these shouldn't exist, but they aren't harmful. They are reported as a warning line (`<commit>\twarning: unreferenced object <hash>`) that doesn't count as a problem and doesn't affect the exit status. *(Confirmed by the user.)*
- **MEDIUM: uninitialized repository.** Fails with the existing `ErrNotInitialized` message. `--revision` and `--all` together are a usage error.
- **LOW: `--revision` naming a non-data commit** (for example a source branch). It fails at manifest read and is reported as that commit's inventory problem.
- **LOW: cancellation (Ctrl-C).** Returns the context error; any partial report is discarded.

## Assumptions

- No `--json` output for now; it can be added later if a CI use appears.
- `--all` follows all parents of merge commits, as `git rev-list` does by default.
- Git's own object-store integrity (`git fsck`) is out of scope. We check RepoDB's content addressing on top of whatever Git returns.

## Resolved questions

1. Unreferenced inventory objects are a warning, not a problem (user, 2026-10-02).
2. `--all` is in scope for this issue (user, 2026-10-02).
