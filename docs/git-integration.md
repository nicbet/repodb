# Git storage and integration decision (M0)

The native-Git path remains the default. In the opt-in M4.3 journal prototype,
sync refuses dirty durable working state and only exchanges intentional data
commits; see [working-state.md](working-state.md).

Status: accepted baseline from the executable M0 experiment on 2026-09-11.
The recorded run used Git 2.55.0 on macOS; the automated experiment remains the
compatibility check for other supported Git versions and operating systems.
The experiment is repeatable with:

```sh
make m0
```

It recreates `/tmp/repodb-m0/` and leaves its bare remote, clones, linked
worktree, packed objects, configuration, and hook log available for inspection.
The automated test runs the same experiment in an isolated temporary directory.

## Storage and ref layout

Use `refs/repodb/data` as the writable local catalog head. A catalog commit has
a Git tree containing `manifest.json` and every immutable schema/data object at
`objects/sha256/<prefix>/<remainder>`. The SHA-256 value in the manifest remains
a RepoDB content identity; the tree entry makes the object reachable to Git.
Git object IDs and RepoDB hashes are separate identities.

Publish objects and the commit before advancing the local data ref. Advance the
ref with `git update-ref refs/repodb/data <new> <expected-old>`. The experiment
proved that one contender succeeds and a stale contender is rejected. Resolving
`git rev-parse --path-format=absolute --git-common-dir` gives the same repository
identity from a main worktree and a linked worktree. Repository locks and caches
must live relative to that common directory.

The complete synthetic catalog survived push, remote repack/prune, fetch into a
fresh clone, local cache deletion, local garbage collection, and `git fsck`.
Neither catalog construction nor ref publication changed source `HEAD`, the
index, or a worktree.

This proves logical reachability and Git's ref compare-and-swap behavior. M1
must still specify filesystem and Git-version assumptions for acknowledged
power-loss durability; atomic ref replacement does not by itself prove that
objects have reached stable storage.

## Enable and transport contract

`repodb enable --remote <remote>` selects one remote and idempotently adds this fetch refspec,
while preserving all existing refspecs:

```text
+refs/repodb/data:refs/repodb/remotes/<remote>/data
```

Fetched state is stored separately from the writable local data head. A fetch
must never replace local database work. `enable` does not add a push refspec.
Adding one either changes what a plain push means or still fails to cover an
explicitly scoped push. RepoDB will not silently change branch publication
semantics to approximate transparent database pushes.

The supported ordinary-command behavior is:

| Command | Source refs | RepoDB data |
| --- | --- | --- |
| `git clone` | Cloned normally | Not fetched until enable |
| `git fetch <remote>` after enable | Fetched normally | Updates only `refs/repodb/remotes/<remote>/data` |
| `git pull --no-rebase` after enable | Fetches, then merges source | Fetches tracking data; does not reconcile it |
| `git pull --rebase` after enable | Fetches, then rebases source | Fetches tracking data; does not reconcile it |
| `git push` | Uses the user's normal push rules | Does not push data |
| `git push <remote> <branch>` | Pushes the selected branch | Does not push data |
| Offline Git operation | Behaves as Git reports | Local committed data remains readable |

An explicit `repodb sync --remote <remote>` is the guaranteed network operation.
It fetches the selected remote, validates and fast-forwards its tracking data
into the local data head when possible, and normally pushes an outgoing
fast-forward. Divergence enters M4's three-way merge and durable conflict
workflow while preserving both heads.

There is no atomicity claim between source and data histories. Even when a remote
supports Git's atomic push capability, ordinary branch commands above do not
select both histories. Status and sync output must report these dimensions
separately:

- local transaction durability;
- incoming data fetched but not reconciled, or a reconciliation conflict;
- outgoing local data not present at the selected remote;
- source branch ahead/behind state; and
- the last transport error.

## Reconciliation and session boundary

`repodb sync` is the reconciliation boundary. Engine open and transaction start
perform no network operation and pin the current writable local head. Autocommit
statements each start at a new transaction boundary.

An explicit transaction remains pinned to its initial immutable snapshot through
commit or rollback, even if `git fetch` or `repodb sync` updates tracking state in
parallel. Its commit uses compare-and-swap against the local data head. If another
transaction or reconciliation advanced that head, the commit returns a retryable
stale-writer conflict rather than replaying arbitrary SQL.

After commit or rollback, the session observes the current local head when its
next transaction begins. This gives existing sessions a precise visibility rule
without letting a fetch change an in-flight transaction.

## Hook decision

Hooks are not part of the correctness path. Git has no general `post-fetch` hook;
`post-merge` misses fetch-only and rebase flows; and `pre-push` cannot add a ref to
the update list already selected by its parent `git push` process.

The experiment demonstrates that a dispatcher can preserve and invoke an
existing `pre-push` hook alongside a RepoDB component, including idempotent
installation. This is technically possible but competes with existing hook
managers and `core.hooksPath`. The initial `enable` implementation therefore
does not install or replace hooks. A later opt-in diagnostic hook may warn about
outgoing unsynchronized data, but it must call the same sync/status API and must
not claim to transport or reconcile data itself.
