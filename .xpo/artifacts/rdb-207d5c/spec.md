# Spec: Windows-safe object writes in `Filesystem.Put`

## What
Stop `Filesystem.Put` from creating read-only object files on Windows, and treat a failed rename as success when an object file already exists at the destination.

## Why
On Windows, `Chmod(0o444)` sets `FILE_ATTRIBUTE_READONLY`, which blocks:
- **Renaming onto an existing object.** Two writers creating the same content-addressed object can both pass the `os.Stat` pre-check. The second rename fails with "Access is denied". Since rdb-6e40a9 it also retries for 2s first, because `ACCESS_DENIED` is treated as possibly transient.
- **Deleting.** `os.Remove` fails on read-only files. That breaks the deferred temp-file cleanup when a rename fails (the temp file leaks), `t.TempDir()` cleanup in tests (`writeLegacyLayout` in `repository_test.go` writes via `Put`), and any manual cleanup of a legacy `.repodb/` directory after `import-legacy`.

Context: `Filesystem` backs the legacy tracked `.repodb/objects` layout. Today only tests write through it; `import-legacy` reads legacy objects directly.

## How
1. **Mark read-only on Unix only.** Wrap the `tmp.Chmod(0o444)` in `if runtime.GOOS != "windows"`, with a comment explaining why. Unix behavior is unchanged. On Windows, content addressing plus the `Get` integrity check already protect objects, and the read-only bit only gets in the way.
2. **An existing destination counts as success.** If the rename (`robustio.Rename`) fails, `os.Stat(path)`. If it's a regular file, return `hash, nil`: another writer won the race, and the content is identical by construction (`Get` still verifies the hash on read). Otherwise return the original rename error. Requiring a regular file means a directory or other junk at the object path still fails loudly.
3. **Testability:** add a package-level `var rename = robustio.Rename` in `filesystem.go`, so a test can simulate "the rename lost a race" on Unix, where rename-over normally succeeds.

## Tests (`common/storage/filesystem_test.go`, new)
- Round trip: `Put` then `Get` returns the same bytes.
- Concurrent writers: 16 goroutines `Put` the same data at once. All succeed with the same hash, no `.repodb-object-*` temp files are left, and `Get` works.
- Lost race: stub `rename` to create the destination and then return an error. `Put` succeeds, and the temp file is cleaned up.
- Real failure: stub `rename` to return an error without creating the destination. `Put` returns that error.
- Unix only (skipped on Windows): the stored object's mode is 0444.

## Acceptance criteria
- Concurrent `Put` of the same object succeeds on Windows: no read-only destination, and a lost race counts as success.
- Objects and leftover temp files can be removed on Windows (no read-only attribute).
- Unix objects are still 0444; `make test` passes (includes the Windows build + vet).

## Decisions
- **Unix-only rather than dropping the chmod:** the issue allows either. Unix-only keeps "Unix behavior unchanged" literally true, and the read-only mode is a cheap guard against accidental edits there.
- **Rename rather than a no-replace primitive:** `os.Link` (fails with EEXIST if the target exists) or `MoveFileEx` without `REPLACE_EXISTING` would avoid replacing entirely. But hard links aren't available on FAT/exFAT and some network shares, and the rename + exists check is portable and simple. With the read-only bit gone, a rename over an identical object on Windows succeeds, or is retried by robustio while a reader briefly holds it.
- **Not handled:** existing read-only object files already on a Windows disk. No release has ever run on Windows, and git checkouts don't preserve the read-only attribute, so none should exist.
