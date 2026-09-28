# Docs: file locking is cross-platform, not POSIX-only

## What

Four places still describe RepoDB's locks as POSIX-only:
- `README.md` Installation: "The current implementation requires POSIX file locking";
- `docs/guide.md` Git interaction: "a POSIX file lock at …/publish.lock";
- `docs/storage-format.md`: "an advisory POSIX file lock", and "requires POSIX `flock`".

Since rdb-52da05, both locks (`publish.lock` and `working.lock`) use `gofrs/flock`: `flock(2)` on Unix and `LockFileEx` on Windows.

## How

Rewrite those passages to say:
- the locks are advisory on Unix (`flock(2)`) and mandatory on Windows (`LockFileEx`), via `gofrs/flock`, and shared by linked worktrees and processes. The lock files are only held, never read or written, so the difference doesn't matter (as recorded in rdb-52da05);
- **platform status, stated exactly:** macOS (APFS, Git 2.55) is the tested baseline. Windows builds and passes `go vet` on every `make test` (rdb-ef8c25) and has Windows-specific file handling (rdb-6e40a9, rdb-207d5c), but the test suite has not been run on Windows. Linux is not separately qualified beyond sharing the Unix code path.

`plan.md` (historical) and the benchmark history files are left alone.

## Acceptance criteria

- `grep -i posix` over README and `docs/` (excluding `plan.md` and `benchmarks/history`) finds no claim that locking requires POSIX.
- The link check passes and `git diff --check` is clean.
