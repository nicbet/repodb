# Walkthrough: Windows support via gofrs/flock

## What was built and why

RepoDB didn't compile for `GOOS=windows`. The only blocker in the product code was the two cross-process locks, which called `unix.Flock` directly. The benchmark code also called `unix.Getrusage`, which blocked `go build ./...` / `go vet ./...` for the whole module. Both are now portable, and the module builds and vets for Windows (amd64 and arm64), Linux and darwin.

## How the pieces fit together

### Locks (`common/repository`)

RepoDB has two lock files under `<git common dir>/repodb/locks/`:

- `publish.lock`, taken by `Repository.lock`, serializes publishing to `refs/repodb/*`.
- `working.lock`, taken by `WorkingState.lock`, serializes writes to the working-state journal. It is layered under the in-process mutex `w.mu`.

Both used the same hand-rolled loop: open the file, try `flock(LOCK_EX|LOCK_NB)`, and on `EWOULDBLOCK` wait 10ms and retry until ctx is done. That pattern is exactly what `gofrs/flock` provides. It uses flock(2) on Unix and LockFileEx on Windows. Both sites now call one helper in `repository.go`:

```go
func acquireFileLock(ctx context.Context, path string) (func(), error)
```

It:
1. Creates a `flock.New(path, flock.SetPermissions(0o600))`, keeping the old file mode.
2. Calls `TryLock()` once, then falls back to `TryLockContext(ctx, 10ms)`, keeping the old poll interval.
3. On failure calls `fl.Close()` to release the fd and returns the error. Cancellation surfaces as `ctx.Err()`, which `TestLockCancellationIsDefiniteRejection` depends on.
4. On success returns `fl.Unlock`, which releases the lock and closes the fd.

`WorkingState.lock` keeps its ordering: take `w.mu`, then the file lock. Unlock releases the file lock, then `w.mu`. Every error path still releases `w.mu`.

**Why the extra `TryLock()` before `TryLockContext`:** gofrs's `tryCtx` checks `ctx.Err()` before its first attempt, so an already-cancelled context would fail even when the lock is free. The old loop tried the lock first, so a free lock was granted regardless of ctx. The single up-front `TryLock()` keeps that behavior, so callers that lock during shutdown or cleanup with a done context behave as before.

**Interoperability:** on Unix, gofrs/flock still uses flock(2) on the same paths, so a new binary and an older binary exclude each other correctly during a rolling upgrade. On Windows, LockFileEx locks are mandatory rather than advisory. That doesn't matter here because the lock files are never read or written, only held.

### Peak RSS in the benchmarks

`integration/sync_bench_test.go` and `experiments/dbbench` report process peak RSS via `getrusage`. That code moved into build-tagged helpers in each package:

- `peakrss_unix[_test].go` (`//go:build unix`): `Getrusage`, with Linux's KiB `Maxrss` scaled to bytes (darwin already reports bytes).
- `peakrss_other[_test].go` (`//go:build !unix`): returns `(0, false)`.

Note that `_unix` is **not** a filename-implied GOOS constraint in Go, so the explicit `//go:build` lines are required.

`peakRSS()` returns `(int64, bool)` so callers can tell "unavailable" apart from "zero". Review found that dbbench originally lost this distinction: `report.PeakRSS` was a plain `int64`, so Windows reports printed `0.0 MiB` and serialized `0`, which looked like a measurement. It is now `*int64` with `omitempty`. When unavailable, the JSON field is omitted and the summary prints `Process peak RSS: unavailable`. When measured, the JSON shape matches the committed `docs/scorecard-*.json` files. `TestReportOmitsUnavailablePeakRSS` covers both cases. The Go benchmark simply skips `ReportMetric` when unavailable.

## Dependencies

- Added `github.com/gofrs/flock v0.13.1` as a direct dependency.
- `golang.org/x/sys` stays; the unix-only helpers and `x/term` still use it.
- `go.sum` also picked up newer testify/yaml hashes. These are test-only transitive dependencies of flock and don't affect RepoDB's build.

## Not covered

This change makes RepoDB compile for Windows. It hasn't been run on a real Windows host. Runtime behaviors like renaming over open files, path handling and git invocation on Windows are unverified and should get their own issues if problems show up.
