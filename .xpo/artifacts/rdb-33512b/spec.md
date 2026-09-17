# Spec: Workflow and operations documentation

## What
Create `docs/guide.md` — a unified user guide that walks through the full RepoDB lifecycle. Link it from README.md's documentation table.

## Why
Existing docs cover individual subsystems (SQL, sync, merge, working state, storage) but there's no single "how to use RepoDB" guide. New users hit the quickstart, then have to piece together behavior from multiple docs.

## How

### Structure of `docs/guide.md`

1. **Setup** — `repodb init`, repo structure, what gets created (`refs/repodb/data`)
2. **Starting the server** — `repodb start` flags (`--addr`, `--persistence`), MySQL client access
3. **Persistence modes** — journal (default in embedded, fast) vs native-git (default in server, every tx in history), how to switch (`--persistence` flag / `engine.Options`), tradeoffs
4. **Data lifecycle** — `status`, `diff`, `commit` (checkpoint), what "dirty" means, when to checkpoint
5. **CLI reference** — full flag reference for all 11 commands (expands on README table)
6. **Synchronization** — `enable`, `sync`, the fetch/merge/push cycle, tracking refs
7. **Conflict resolution** — `conflicts`, `resolve`, resolution choices, distributed identity guidance
8. **Embedded Go API** — open/session/exec/query pattern, persistence options, pointer to sql-m2.md
9. **Backup and recovery** — what to copy (git repo + journal), cache is disposable, `RecoverCommit`, `import-legacy`
10. **Git interaction** — what ordinary git commands do and don't do to RepoDB data, no hooks installed

### README.md changes
Add a row to the documentation table pointing to the new guide.

## Acceptance criteria
- [ ] `docs/guide.md` exists with all 10 sections
- [ ] All CLI commands documented with their flags
- [ ] Persistence mode switching clearly explained
- [ ] README documentation table updated
- [ ] No factual errors vs. the source code and existing docs
