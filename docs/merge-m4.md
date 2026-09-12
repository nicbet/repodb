# M4 merge and conflict contract

RepoDB synchronizes divergent data histories with a deterministic three-way
merge. It finds the Git merge base of the local and fetched data commits, reads
all three validated snapshots, and compares catalog entries and rows by their
encoded primary key.

For each table or row, RepoDB accepts an identical result, a change made on only
one side, or changes to different primary keys. An insert of the same key with
the same value is also identical. Competing updates, insert/insert collisions
with different values, and update/delete pairs become row conflicts. Rows are
the smallest merge unit in M4; fields are not combined.

A table created differently on both sides, delete/modify combinations, and
competing schema changes become schema conflicts. The current SQL scope has no
`ALTER TABLE` or secondary unique indexes. Primary-key collisions exercise the
supported unique constraint, and every merged row is decoded and checked for
type, width, `NOT NULL`, and primary-key consistency before publication.

Successful reconciliation creates a commit with the local and remote tips as
its two parents. RepoDB publishes it through the same repository-wide lock and
expected-head update used by SQL commits. It pushes the exact candidate commit
with an ordinary, non-force push. A local publication race or rejected remote
push causes a fresh fetch and merge, bounded to three attempts. Exhaustion keeps
the local head and fetched tracking ref intact and reports both.

## Inspecting and resolving conflicts

An unresolved merge never changes `refs/repodb/data`. RepoDB writes an atomic
conflict record under the shared Git directory at
`repodb/conflicts/<remote>.json`. The record contains the base, local, and remote
heads plus each conflicting row or schema value, so it survives process and
engine restarts.

Use:

```sh
repodb conflicts --remote origin
repodb resolve --remote origin --id '<conflict-id>' --take local
```

Choices are `local`, `remote`, `base`, and `delete`. A row choice selects the
whole encoded row. A schema choice selects the whole table state from that side;
`delete` removes it. Choices are saved individually. When all conflicts have a
choice, RepoDB rechecks the recorded heads, validates the result, publishes the
merge, and resumes synchronization. If either side advanced, the next sync
derives a new conflict set instead of applying stale choices.

Conflict files are local coordination state. The referenced commits and both
histories remain in Git; completed resolution removes the local conflict file.

## Distributed row identity

The primary key is the row identity. Independently allocated values can collide,
so a clone-local auto-increment counter would not be safe. Auto-increment is not
supported in the current SQL scope. Applications that create rows offline should
use identifiers that are unique across writers, such as UUIDs or a composite key
containing a stable writer identity. A collision remains an explicit row conflict
and is never silently renumbered.

After a merge or resolution reaches both clones, another sync is idempotent and
does not create another commit.
