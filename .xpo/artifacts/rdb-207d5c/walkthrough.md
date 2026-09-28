# Walkthrough: Windows-safe object writes in `Filesystem.Put`

## What was built and why

`Filesystem` is the content-addressed object store behind the legacy tracked `.repodb/objects` layout (today only tests write through it; `import-legacy` reads those files directly). `Put` writes each object atomically: temp file, `Chmod(0o444)`, rename into place.

On Windows that `0o444` becomes `FILE_ATTRIBUTE_READONLY`, which has two effects Unix doesn't have:

1. **You can't rename onto a read-only file.** `Put` checks with `os.Stat` before writing, but two writers creating the same object can both pass that check. The second rename then hits the first writer's read-only file and fails with "Access is denied". After rdb-6e40a9 it also retried that for 2s first, because `ACCESS_DENIED` is treated as possibly transient.
2. **You can't delete a read-only file.** That broke `Put`'s own deferred temp-file cleanup when a rename failed, `t.TempDir()` cleanup in tests that write through `Put` (`writeLegacyLayout`), and manual removal of a legacy `.repodb/` after import.

## The two changes

### 1. Read-only only on Unix

```go
if runtime.GOOS != "windows" {
	if err := tmp.Chmod(0o444); err != nil { ... }
}
```

On Unix nothing changes: objects are still 0444, a cheap guard against accidental edits, and 0444 doesn't block rename or unlink there because those depend on the directory's permissions. On Windows the bit is dropped. Integrity doesn't depend on it: an object's path *is* its hash, and `Get` recomputes the hash on every read. Without the attribute, a rename onto an identical existing object just replaces it, as on Unix. If a reader has it open at that moment, robustio (rdb-6e40a9) retries until the reader closes it.

I used a runtime check rather than build-tagged files because it's one branch in one function, and the comment next to it explains why.

### 2. A lost race counts as success

```go
if err := rename(tmpName, path); err != nil {
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
		return hash, nil
	}
	return "", err
}
```

If the rename fails but an object file is now at the destination, another writer stored the same content first. Content addressing means its bytes are identical to ours, so `Put` has done its job. This is the same trust the existing `os.Stat` pre-check already places in an existing file, just applied after the race instead of before. The `IsRegular()` check stops a directory or other junk at the object path from being mistaken for success; that still returns the rename error. The deferred `os.Remove(tmpName)` cleans up our temp file in both cases, which now also works on Windows.

### Testability seam

`var rename = robustio.Rename` is a package variable so tests can reproduce the Windows-only failure (rename fails because the target exists) on Unix, where renaming over an existing file never fails.

## Tests (`common/storage/filesystem_test.go`, new)

- **Round trip:** `Put` then `Get`.
- **Concurrent same-object `Put`:** 16 goroutines all succeed with the correct hash, no `.repodb-object-*` temp files are left behind, and `Get` works. On Unix this is a smoke test, since renames never collide; on Windows it exercises the real race.
- **Lost race:** a stubbed `rename` writes the destination and then returns an error. `Put` succeeds and the temp file is removed. A mutation check confirmed this test fails when the lost-race branch is turned off.
- **Real failure:** a stubbed `rename` fails without creating the destination, and the error is returned (`errors.Is`).
- **Unix mode:** the stored object is 0444 (skipped on Windows).

## Alternatives considered

- **Drop the chmod everywhere:** simpler, but it changes Unix behavior for no gain.
- **A rename that refuses to replace** (`os.Link`, which fails if the target exists, or `MoveFileEx` without `REPLACE_EXISTING`): this avoids replacing at all, but hard links aren't available on FAT/exFAT and some network shares, and it needs platform-specific code. Rename plus the existence check is portable.

## Not handled

Read-only object files that are already on a Windows disk. No version before this one ran on Windows, and git checkouts don't preserve the read-only attribute, so none should exist.
