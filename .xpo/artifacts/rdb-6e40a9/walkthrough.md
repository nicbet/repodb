# Walkthrough: Windows-robust rename/remove

## What was built and why

RepoDB writes some files by atomic replace: write a temp file, then rename it over the real path. On Unix that's always safe, because rename swaps the directory entry and any reader keeps reading the old inode. On Windows, Go opens files with `FILE_SHARE_READ|FILE_SHARE_WRITE` but **not** `FILE_SHARE_DELETE` (see `syscall.Open` in Go 1.27). While any process has the target open, even just an `os.ReadFile`, renaming onto it or removing it fails with `ERROR_SHARING_VIOLATION` or `ERROR_ACCESS_DENIED`. Readers only hold these files for microseconds, so the fix is to retry briefly.

## How the pieces fit together

### `common/robustio`

The package is split into three files, so the retry logic can be tested on every platform while the retry only happens on Windows:

- **`robustio.go`**: `retry(op, transient)` is the loop, with no platform code. It returns immediately on success or on an error `transient` rejects. Otherwise it sleeps and tries again. The first sleep is 1ms, and each later sleep grows by a random amount up to its current value (roughly doubling with jitter), so concurrent retriers don't move in lockstep. It stops once elapsed time plus the next sleep would reach `timeout` (2s) and returns the last error unchanged, so callers' `errors.Is` checks still work. The timer starts after the first failure, so the success path costs nothing.
- **`robustio_windows.go`**: `Rename`/`Remove` wrap `os.Rename`/`os.Remove` in `retry` with `isTransient`. That uses `errors.As` to pull the `syscall.Errno` out of the `*os.LinkError`/`*os.PathError` and matches `ERROR_ACCESS_DENIED` (5) and `ERROR_SHARING_VIOLATION` (32).
- **`robustio_other.go`** (`!windows`): `Rename`/`Remove` are one-line pass-throughs, so Unix behavior is identical to before.

This mirrors Go's own `cmd/internal/robustio`, which the toolchain uses for the same Windows problem. The 2s bound comes from there too. One deliberate difference: Go also retries `ERROR_FILE_NOT_FOUND` on rename, but here the rename source is our own just-closed temp file, so "not found" is a real error and retrying would only delay it.

### Call sites

- `integration/conflicts.go`
  - `saveConflictSet`: the final rename onto `conflicts/<remote>.json`. A concurrent `repodb conflicts`/`resolve` reading the file via `loadConflictSet` could hold it open.
  - `clearConflictSet`: removing that file, for the same reason. Its existing `ErrNotExist → nil` handling still works because the error is returned unwrapped.
- `common/storage/filesystem.go` `Put`: the rename of a new object into place. A concurrent `Get` could be reading the target.

The deferred `os.Remove(tmp)` cleanups were left alone. Nothing else opens those temp files, and after a successful rename they no longer exist.

## Testing

`robustio_test.go` is an internal test (package `robustio`), so it can call `retry` directly with a fake op and predicate:
- It succeeds after several transient failures.
- It makes exactly one call when the error isn't transient.
- It gives up and returns the last transient error, with `timeout` shortened to 50ms to keep the test fast and a loose upper bound on elapsed time so it isn't flaky.

The Windows errno predicate can't run on macOS. `make test` compile-checks it through the `cross-windows` target (rdb-ef8c25).

## Known interaction

`ERROR_ACCESS_DENIED` is also what Windows returns when renaming onto an existing **read-only** file, and `Put` marks objects 0444 (rdb-207d5c). If two writers race to create the same object, the loser's rename now retries for up to 2s before failing with the same error it returned before this change. rdb-207d5c removes that case.
