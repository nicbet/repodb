# Walkthrough: Workflow and operations documentation

## What changed

Created `docs/guide.md` — a comprehensive user guide covering the full RepoDB lifecycle. Added a link to it from the README documentation table.

### Guide structure

1. **Setup** — `repodb init`, prerequisite of an existing Git repo, what gets created
2. **Starting the server** — `repodb start` with full flag table (`--addr`, `--repo`, `--persistence`), MySQL client access
3. **Persistence modes** — journal vs native-git, tradeoffs, switching modes (CLI flag and embedded API), checkpointing, mixed-mode guard
4. **Data lifecycle** — `status`/`diff`/`commit` workflow, what "dirty" means
5. **CLI reference** — summary table of all 11 commands with key flags, then detailed subsections with flag tables for `sql`, `enable`, `sync`, `conflicts`, `resolve`, `import-legacy`
6. **Synchronization** — the enable → sync → resolve workflow, what ordinary git commands do/don't do to RepoDB data
7. **Conflict resolution** — three-way merge behavior, auto-accepted vs conflicting changes, distributed identity guidance (nuanced: auto-increment is fine for single-writer, prefer UUIDs across clones)
8. **Embedded Go API** — open/session pattern, snapshot isolation, implicit commit on DDL, recovery
9. **Backup and recovery** — what to copy (git repo + journal), cache is disposable, worktree sharing, legacy import
10. **Git interaction** — ref layout, publication lock, integrity checks, SHA-1/SHA-256 support

### README change

Added one row to the documentation table: `[User guide](docs/guide.md)` with description "Setup, CLI reference, persistence modes, sync workflow, and backup".

## Why

The existing docs (sql-m2, sync-m3, merge-m4, working-state, storage-format) cover individual subsystems but there was no unified guide for new users after the README quickstart.

## Decisions

- Placed the guide link first in the README documentation table since it's the natural next step after the quickstart.
- Didn't duplicate the quickstart examples — the guide references the README and covers operational depth instead.
- Documented current behavior for `--remote` (required on every command) even though we filed `rdb-bc2820` to default it from saved config.
- Nuanced the distributed identity guidance to say auto-increment is fine for single-writer setups, since `rdb-a2d28e` will add AUTO_INCREMENT support.

## Related issues filed during review

- `rdb-3c8ef9` — `repodb sync` should prompt to checkpoint when journal is dirty
- `rdb-bc2820` — Default `--remote` flag to saved `repodb.remote` config
