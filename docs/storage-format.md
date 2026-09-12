# Git snapshot format and durability (M1/M2)

Status: format version 1 baseline, implemented 2026-09-11.

## Authoritative state

The repository-wide live catalog head is `refs/repodb/data`. Its target is a
commit whose tree has this shape:

```text
manifest.json
objects/
  sha256/
    ab/
      cdef...  # immutable content whose RepoDB SHA-256 is abcdef...
```

`manifest.json` contains `format_version`, `default_database`, the table catalog,
and a sorted, unique `objects` array. The array is the complete inventory of
RepoDB-addressed objects in that snapshot. Every inventory entry must have one
tree entry at its derived path, and the tree may not contain unlisted files.
Schema and data roots named by a table must occur in the inventory.

On open, RepoDB checks the format, manifest shape, inventory ordering, exact tree
membership, and SHA-256 content of every object. A snapshot is rejected as
corrupt before it becomes available to a caller. The M2 SQL engine additionally
validates the Prolly object graph and schema; the repository layer treats object
contents as opaque bytes.

The Git commit has the previous data head as its sole parent. Initial snapshots
have no parent. Commits use `RepoDB <repodb@localhost>` as author and committer
and `RepoDB snapshot v1` as the subject. Commit timestamps come from Git at
publication time. Source commits and data commits have independent histories.

## Publication and concurrency

A snapshot writer pins its base commit and implements the content-addressed
`storage.Store` interface. Publication performs these operations in order:

1. Read and verify every object retained from the pinned base snapshot.
2. Validate new object hashes, catalog roots, and the complete inventory.
3. Acquire the repository publication lock at
   `<git-common-dir>/repodb/locks/publish.lock`.
4. Write Git blobs, a temporary-index tree, and a data commit.
5. Run `git update-ref refs/repodb/data <new> <expected-old>`.
6. Reload and verify the newly published snapshot before returning success.

The lock is an advisory POSIX file lock shared by linked worktrees and processes.
The expected old ref value remains the authority: concurrent writers based on
the same snapshot yield exactly one success, while every stale writer receives
`repository.ErrConflict`. A failed or interrupted operation before `update-ref`
can leave unreachable objects for Git to collect, but cannot expose a partial
snapshot. Readers pin immutable commits and need no lock.

Snapshot construction uses a temporary index under
`<git-common-dir>/repodb/tmp`; it never reads or changes the user's index.
Rebuildable caches belong under `<git-common-dir>/repodb/cache/v1` and are never
authoritative. Deleting that directory cannot affect snapshot reads.

## Durability assumptions

Object, tree, commit, and ref writes invoke Git with:

```text
-c core.fsync=committed,reference -c core.fsyncMethod=fsync
```

RepoDB reports a normal successful publication only after the compare-and-swap
`update-ref` process exits and the resulting snapshot passes integrity checks.
If the ref advanced but a later check fails, the commit error explicitly carries
a committed outcome and candidate ID. This baseline was exercised with Git
2.55.0 on a local macOS filesystem. The current
implementation requires POSIX `flock`. Automated round trips cover both Git's
SHA-1 and SHA-256 repository object formats; RepoDB content identities remain
SHA-256 in either case.

`reference` is explicit because Git documents it as a separate component; the
name `committed` alone must not be read as proof that ref hardening is enabled.
The durability claim assumes Git honors those fsync settings and the filesystem
and storage device honor `fsync` and atomic ref replacement. Filesystems with
weaker persistence or locking semantics require separate qualification. RepoDB
does not claim atomic publication between the source branch and the data ref.

## Repository states and recovery

- No `refs/repodb/data`: `repository.ErrNotInitialized`.
- A tracked `.repodb/config.json` or `.repodb/manifest.json` without a data ref:
  `repository.ErrLegacyLayout`, with instructions to run `repodb import-legacy`.
- Unsupported `format_version`: an explicit unsupported-format error.
- Missing, extra, malformed, or hash-mismatched snapshot content:
  `repository.ErrCorrupt`.
- A stale expected head: `repository.ErrConflict`; the current live snapshot is
  unchanged.

`repodb import-legacy [path]` verifies the prototype filesystem object store and
publishes it as the initial data commit. It refuses to overwrite an existing
data ref and deliberately retains `.repodb/` so migration is reviewable and
recoverable. Users may remove those legacy files in a separate source commit
after checking the imported snapshot.
