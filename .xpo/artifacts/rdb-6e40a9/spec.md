# Spec: Windows-robust rename/remove for atomic-replace sites

## What
Add a tiny `common/robustio` package with `Rename(oldpath, newpath)` and `Remove(path)`. On Windows they retry briefly on transient sharing errors; everywhere else they are exactly `os.Rename` / `os.Remove`. Use them at the atomic-replace sites listed in the issue.

## Why
Go on Windows opens files with `FILE_SHARE_READ|FILE_SHARE_WRITE` only (confirmed in Go 1.27 `syscall.Open`), so while another process has the target open (for example `os.ReadFile` in `loadConflictSet` or `Filesystem.Get`), renaming onto it or removing it fails with `ERROR_SHARING_VIOLATION` / `ERROR_ACCESS_DENIED`. Readers hold files open for microseconds, so a short retry succeeds.

## How
1. **`common/robustio/robustio.go`** (all platforms): the retry loop, modeled on Go's `cmd/internal/robustio`:
   - `retry(op func() error, transient func(error) bool) error`
   - Stops immediately on success or a non-transient error.
   - Backoff: starts at 1ms and grows by a random amount up to the current sleep (jittered, roughly doubling); gives up once total elapsed time plus the next sleep would reach **2s** (`timeout`, the same bound as Go's toolchain). Returns the last error, unwrapped.
2. **`robustio_windows.go`**: `transient` matches `syscall.Errno` values `ERROR_ACCESS_DENIED` (5) and `ERROR_SHARING_VIOLATION` (32, from `golang.org/x/sys/windows`) via `errors.As` (the errno is wrapped in `*os.LinkError` / `*os.PathError`). `Rename`/`Remove` go through `retry`.
3. **`robustio_other.go`** (`//go:build !windows`): `Rename`/`Remove` call `os.Rename`/`os.Remove` directly with no retry.
4. **Call sites:**
   - `integration/conflicts.go`: `saveConflictSet` final `os.Rename(name, path)` → `robustio.Rename`; `clearConflictSet` `os.Remove(conflictPath(...))` → `robustio.Remove` (the `ErrNotExist` → nil handling stays as it is).
   - `common/storage/filesystem.go`: `Put`'s `os.Rename(tmpName, path)` → `robustio.Rename`.
   - Deferred temp-file cleanups (`defer os.Remove(tmp)`) stay unchanged. Nothing else opens those temp files, and after a successful rename they don't exist.

## Tests
`retry` is platform-independent, so it's unit-tested on every OS with a fake op and predicate:
- Succeeds after N transient failures.
- Returns immediately (1 call) on a non-transient error.
- Gives up within the bound and returns the last transient error.

To keep the give-up test fast, `timeout` is a package variable the test shortens. The Windows errno predicate is compile-checked by `make cross-windows`; it can't be run here.

## Acceptance criteria
- Renaming onto `conflicts/<remote>.json` and onto object files is retried on Windows when a reader briefly holds the target open.
- Retry is bounded (2s) and applies only on Windows; Unix code paths call `os.Rename`/`os.Remove` directly.
- `make test` passes (includes Windows build + vet).

## Decisions
- **Which errors are transient:** only `ACCESS_DENIED` and `SHARING_VIOLATION`, as the issue specifies. Go's robustio also retries `ERROR_FILE_NOT_FOUND` for rename, but here the source is our own just-closed temp file, so "not found" is a real error. For `Remove`, not-found is already treated as success by the caller.
- **Package `common/robustio`:** both `common/storage` and `integration` need it, and `integration` already imports `common/*`. The name follows Go's own package.
- **Overlap with rdb-207d5c:** `ACCESS_DENIED` is also what Windows returns when the rename target is a read-only (0444) object that already exists. That isn't transient, so until rdb-207d5c lands, `Put` in that case now waits up to 2s before returning the same error it returns today. rdb-207d5c removes the case, since an existing destination counts as success, so no extra handling is added here.
