# Spec: skip client-side hooks on RepoDB data-ref pushes

## What
Pass `--no-verify` to `git push` in `CLI.PushRef` and `CLI.PushCommit` (`common/git/git.go`).

## Why
A plain push runs the host repo's `pre-push` hook. Tools that sync RepoDB from a pre-push hook recurse forever, and even without recursion the user's hooks (linters, test gates) shouldn't gate internal bookkeeping refs.

## How
- Add `--no-verify` to both push invocations. A repo-wide search found no other production push call sites (the rest are tests and the `experiments/gitstorage` prototype, which is left alone).
- Fetch is unaffected: no client-side hook runs on fetch.
- Server-side hooks (`pre-receive`, `update`) still run; `--no-verify` only skips client-side `pre-push`, so remote policy is still enforced.

## Acceptance criteria
- New test in `common/git`: temp repo + bare remote, `pre-push` hook that appends to a marker file and exits 1. `PushRef` and `PushCommit` both succeed, the remote has the ref at the expected commit, and the marker file doesn't exist.
- `make test` passes (including the Windows cross-compile/vet).
