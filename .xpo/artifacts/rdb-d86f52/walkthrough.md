# Walkthrough: docs describe cross-platform locking accurately

## The problem

Since rdb-52da05, both RepoDB locks (`publish.lock` for ref publication and `working.lock` for the journal) are taken through `gofrs/flock`: `flock(2)` on Unix and `LockFileEx` on Windows. Four doc passages still said locking required POSIX `flock`.

## Changes

- **README, Installation:** replaced "requires POSIX file locking" with the actual platform status.
- **`docs/guide.md`, Git interaction:** the publish lock is "a file lock (`flock(2)` on Unix, `LockFileEx` on Windows)", shared across worktrees and processes. The issue didn't list this passage; it turned up during the search.
- **`docs/storage-format.md`, publication:** the lock goes through `gofrs/flock`, advisory on Unix and mandatory on Windows. That doesn't matter, because lock files are only held, never read or written (as recorded in rdb-52da05). `working.lock` works the same way.
- **`docs/storage-format.md`, durability baseline:** the "requires POSIX `flock`" sentence is replaced with the qualification status.

## Stating platform support precisely

The records of the Windows work (rdb-6e40a9, rdb-ef8c25) show Windows code is compile- and vet-checked on every `make test` but has never been run. So the docs say:
- macOS with Git 2.55 is the tested baseline;
- Windows builds, vets, and has Windows-specific rename and removal handling, but the suite hasn't run there;
- Linux shares the Unix code path and isn't separately qualified.

This avoids replacing one inaccuracy ("POSIX required") with another ("Windows supported").

`docs/plan.md` is a historical record and was left unchanged.
