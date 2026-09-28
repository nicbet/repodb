# Spec: Windows support via gofrs/flock

## What
Make RepoDB compile (build + vet, including tests) for `GOOS=windows` by replacing direct `unix.Flock` calls with `github.com/gofrs/flock`.

## Why
`GOOS=windows go build ./...` fails in `common/repository` on `unix.Flock`, `LOCK_EX`, `LOCK_NB`, `LOCK_UN`, `EWOULDBLOCK`. Nothing else in the lock design is Unix-specific.

## How
1. **`Repository.lock`** (`common/repository/repository.go`, publish lock): keep `MkdirAll(locks/)`; delegate to a shared `acquireFileLock(ctx, path)` helper built on `flock.New(path, flock.SetPermissions(0o600))`. The helper calls `TryLock()` once, then `TryLockContext(ctx, 10*time.Millisecond)`. On failure it closes the flock and returns the error (ctx cancellation surfaces as `ctx.Err()`, preserving `TestLockCancellationIsDefiniteRejection`). Release = `fl.Unlock()` (unlocks and closes the fd).
2. **`WorkingState.lock`** (`working.go`, working lock): same helper, keeping the in-process `w.mu` acquire-before / release-after-file-unlock ordering and releasing `w.mu` on every error path.
3. **Test** `repository_test.go`: hold the publish lock with `flock.New(...).TryLock()` instead of `unix.Flock`.
4. **Remaining `x/sys/unix`** (`unix.Getrusage` for max-RSS reporting in `integration/sync_bench_test.go`, `experiments/dbbench/main.go`): move into build-tagged helpers (see Decisions).
5. `go get github.com/gofrs/flock`; `go mod tidy`.

## Behavior preserved
- Same lock file paths and 0600 permissions; same 10ms poll interval.
- A free lock is granted even if ctx is already done (matches the old loop, which tried before checking ctx).
- On Unix gofrs/flock uses flock(2), so it interoperates with any older binary still holding a lock on the same file.
- On Windows LockFileEx is mandatory, but lock files are never read or written, so that doesn't matter.

## Acceptance criteria
- `go build ./...`, `go vet ./...`, `go test ./...` pass on darwin.
- `GOOS=windows GOARCH=amd64 go build ./...` and `GOOS=windows go vet ./...` pass.
- No `unix.Flock` left in the repo.
- When peak RSS is unavailable, dbbench prints "unavailable" and omits `process_peak_rss_bytes` from its JSON report instead of reporting 0.

## Out of scope
Runtime validation on a real Windows host (path, rename-over-open-file, and git behaviors). This change is about compiling; runtime issues would be tracked separately.

## Decisions
- **Cross-platform max-RSS in the benchmarks** (user-confirmed): use small build-tagged helpers (`peakrss_unix[_test].go` with `//go:build unix`, `peakrss_other[_test].go` with `//go:build !unix`) in each package. `peakRSS()` returns `(bytes, ok)`, and non-unix platforms return `ok=false`. Both packages still build on every platform.
- **Unavailable RSS must not look like a measurement** (review feedback): `report.PeakRSS` is `*int64` with `omitempty`. When `peakRSS()` is not ok, it stays nil, so the JSON field is omitted and the text summary prints `Process peak RSS: unavailable`. Existing scorecard JSON (with the field present) is unchanged in shape. The Go benchmark just skips `ReportMetric` when unavailable, which already reports nothing rather than zero.
