# Walkthrough: RepoDB data pushes skip client-side hooks

## The problem
RepoDB publishes its data ref (`refs/repodb/data`) with `git push` through `CLI.PushRef` and `CLI.PushCommit` in `common/git/git.go`. A plain `git push` runs the host repository's `pre-push` hook. That hook belongs to the user and exists to check *their* pushes: linters, test gates, and sync tools.

`pre-push` is also the natural place for a tool built on RepoDB (nate, xpo) to trigger a sync. The chain then loops: `git push` runs the hook, the hook runs `nate sync`, the sync's data push runs the hook again, and so on. One user push produced 11 nested sync processes in about 6 seconds, and the data ref never reached the remote.

Even without recursion, running user hooks on RepoDB's data ref is wrong. They are slow, they can fail for reasons unrelated to RepoDB, and when they fail they block the data sync.

## The change
Both push invocations now pass `--no-verify`:

```go
run(ctx, root, nil, nil, "push", "--no-verify", remote, source+":"+destination) // PushRef
run(ctx, root, nil, nil, "push", "--no-verify", remote, commit+":"+destination) // PushCommit
```

These are the only places RepoDB pushes on its own behalf. The other `git push` calls in the repo are in tests or in the `experiments/gitstorage` prototype. `FetchRef` needed no change because Git runs no client-side hooks on fetch.

## What `--no-verify` does and doesn't do
- It skips the **client-side** `pre-push` hook only.
- **Server-side** hooks (`pre-receive`, `update`, `post-receive`) still run on the remote. Remote policy on who may update `refs/repodb/*` is still enforced.
- Fast-forward checks are unchanged. `PushCommit` still relies on Git rejecting a push that isn't a fast-forward.

## Test
`common/git/git_test.go` → `TestDataPushesSkipPrePushHook`:
1. Creates a local repo and a bare `origin`, and installs a `pre-push` hook that appends to a marker file and then exits 1. A hook that both rejects the push and leaves a trace catches either failure mode: the push failing, or the hook running at all.
2. Builds a data commit with the package's own plumbing (`HashObjects` → `WriteTree` → `CommitTree` → `UpdateRef`), then calls `PushRef` and asserts the remote ref with `RemoteRef`.
3. Builds a child commit and calls `PushCommit`, then asserts the remote ref moved to it.
4. Asserts the marker file doesn't exist.

Without the fix, the test fails at `PushRef`, where the hook rejects the push. With the fix, it passes.

## Downstream
Tracked in nate as `nate-d00caf`. Consumers that sync from `pre-push` pick up the fix when they update their RepoDB dependency.
