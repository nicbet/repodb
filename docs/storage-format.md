# Git snapshot format and durability (M1/M2)

This remains the default native-Git persistence path. M4.3's opt-in authoritative
journal and intentional checkpoint protocol are documented in
[working-state.md](working-state.md); they do not change the default yet.

Status: format version 2 (2026-09-28): order-preserving keys. Format version 1
was the 2026-09-11 baseline. RepoDB is alpha and does not migrate between
formats: opening a format 1 data history fails with an unsupported-format error
asking to re-initialize it.

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
and `RepoDB snapshot v<format version>` as the subject (currently
`RepoDB snapshot v2`). Commit timestamps come from Git at publication time.
Source commits and data commits have independent histories.

## Key encoding (format 2)

Rows live in Prolly trees keyed by their primary key; secondary indexes are
Prolly trees keyed by index columns. Keys are the concatenation of one
self-delimiting, order-preserving encoding per column (`engine/keycodec.go`),
so `bytes.Compare` on keys agrees with SQL order. Range predicates and
`ORDER BY … LIMIT` on index columns therefore seek into the tree and stream
rows in order instead of scanning the table.

| Column type | Encoding |
| --- | --- |
| signed integers | 8-byte big-endian, sign bit flipped |
| unsigned integers | 8-byte big-endian |
| ENUM | 2-byte big-endian index |
| FLOAT/DOUBLE | 8-byte IEEE-754 bits, all flipped when negative, else the sign bit flipped; −0 normalized |
| DECIMAL | sign class byte, then for non-zero values a sign-flipped 4-byte exponent and digit bytes ending in `0x00`, bitwise inverted for negatives; `1.0` and `1.00` encode equally |
| DATE/DATETIME/TIMESTAMP, TIME | 8-byte microseconds since the Unix epoch (UTC) or duration microseconds, sign bit flipped |
| CHAR/VARCHAR/TEXT | UTF-8 bytes for the default binary collation, else 4-byte big-endian collation weights per rune; CHAR ignores trailing spaces; `0x00` escaped as `0x00 0xFF`, terminated by `0x00 0x01` |
| BINARY/VARBINARY/BLOB | raw bytes, escaped and terminated like strings |

A secondary-index key writes `0x00` for a NULL column or `0x01` plus the column
encoding, so NULLs sort first, and appends the primary key when the index is not
unique or a column is NULL. Keys are never decoded; row values are stored in the
row blob. Format 1 keys (`type, length, decimal text`) were not
order-preserving, which limited lookups to exact points.

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

The lock is shared by linked worktrees and processes. It is taken through
`gofrs/flock`: an advisory `flock(2)` lock on Unix and a mandatory `LockFileEx`
lock on Windows. The lock file is only held, never read or written, so the
difference does not matter. The working-state journal lock (`working.lock`)
works the same way.
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
2.55.0 on a local macOS filesystem. Windows builds and passes `go vet` on every
`make test`, and has Windows-specific rename and removal handling, but the test
suite has not been run on Windows. Linux shares the Unix code path and is not
separately qualified. Automated round trips cover both Git's
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
- Unsupported `format_version`: an explicit unsupported-format error.
- Missing, extra, malformed, or hash-mismatched snapshot content:
  `repository.ErrCorrupt`.
- A stale expected head: `repository.ErrConflict`; the current live snapshot is
  unchanged.
