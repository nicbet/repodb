# Spec: repodb sync — prompt to checkpoint when journal is dirty

## What

When `repodb sync` detects a dirty journal (uncheckpointed working-state changes), and the CLI is running interactively (TTY on stdin), prompt the user to auto-checkpoint before proceeding. If the user confirms, checkpoint with an auto-generated message and continue the sync. If they decline, abort with the existing error. In non-interactive mode (no TTY), keep the current hard-fail behavior unchanged.

## Why

Users who don't yet understand the checkpoint/sync relationship get a confusing error from `requireCleanWorkingState` telling them to run `repodb diff` and `repodb commit -m ...` manually. An interactive prompt collapses two commands into one confirmation.

## Acceptance Criteria

1. Interactive TTY + dirty journal → user sees a prompt like:
   `Journal has uncommitted changes at generation N. Checkpoint before syncing? [y/N]`
2. User answers `y`/`Y` → auto-checkpoint runs with message `"checkpoint before sync"`, then sync proceeds normally.
3. User answers anything else (or empty) → sync aborts with the existing `ErrWorkingDirty` error (no behavior change).
4. No TTY (piped, cron, CI) → sync aborts with the existing error (no behavior change).
5. If the auto-checkpoint itself fails, the error propagates and sync does not proceed.
6. Existing `integration.Sync` function signature and behavior unchanged — it remains a pure library function with no interactive I/O.
7. Existing test `TestSyncRefusesDirtyJournalWorkingState` continues to pass (it calls `integration.Sync` directly, no TTY).

## Flow

1. **Add TTY detection** — use `golang.org/x/term.IsTerminal(int(os.Stdin.Fd()))`. Add `golang.org/x/term` to `go.mod`.

2. **Pre-check in CLI `sync` handler** (`cmd/repodb/main.go`, ~line 148) — before calling `integration.Sync`:
   - Open the repository and working state.
   - Call `working.Status(ctx)` to check `Dirty`.
   - If not dirty, proceed to `integration.Sync` as before.
   - If dirty AND `term.IsTerminal(stdin)`:
     - Print prompt to stderr: `Journal has uncommitted changes at generation %d. Checkpoint before syncing? [y/N] `
     - Read one line from stdin via `bufio.NewReader(os.Stdin).ReadString('\n')`.
     - If input is `y` or `Y` (trimmed), call `working.Checkpoint(ctx, "checkpoint before sync")` and print the commit hash.
     - Otherwise, return the same `ErrWorkingDirty` error that `requireCleanWorkingState` would produce.
   - If dirty AND no TTY: fall through to `integration.Sync` which will return `ErrWorkingDirty` via `requireCleanWorkingState` as before.

3. **No changes to `integration/integration.go`** — `Sync()` and `requireCleanWorkingState()` remain untouched.

4. **Test** — add a test in `cmd/repodb/main_test.go` (or integration test) that verifies the non-interactive path still fails with `ErrWorkingDirty`. The interactive prompt path is hard to unit-test (requires stdin mocking) — verify manually.

## Decisions

- **Prompt at CLI layer, not integration layer** — `integration.Sync` is a library function used programmatically. Interactive I/O belongs in `cmd/repodb/main.go`. This keeps the integration package clean.
- **Default to No (`[y/N]`)** — checkpoint is a write operation; the safe default is to not proceed, matching Unix convention for destructive-ish prompts.
- **Auto-generated commit message `"checkpoint before sync"`** — keeps it simple. The user can always `repodb log` to see what was checkpointed. Not worth prompting for a message in addition to the y/n.
- **`golang.org/x/term` over `mattn/go-isatty`** — `golang.org/x/term` is the stdlib-adjacent choice and `golang.org/x/sys` is already a dependency, so the module graph stays in the `golang.org/x` family.
- **Prompt on stderr, read from stdin** — stderr for the prompt so it doesn't pollute piped stdout. stdin for the answer so it works with normal terminal interaction.

## Edge Cases

- **MEDIUM: Dirty state appears between checkpoint and sync** — After auto-checkpoint succeeds, another process could dirty the journal before `integration.Sync` runs. This is the same race window as manual `repodb commit && repodb sync`. The mid-sync `requireCleanWorkingState` checks will catch it. No special handling needed.
- **LOW: Checkpoint succeeds but sync fails** — The checkpoint is a valid operation regardless. The user now has a clean journal and can retry sync. No rollback needed.
- **LOW: stdin EOF** — `ReadString` returns EOF → treated as "no" → existing error behavior. Fine.

## Assumptions

- `golang.org/x/term` is compatible with the Go 1.27 toolchain in `go.mod`.
- The `repository.OpenWorkingState` + `working.Status` path is cheap enough to call twice (once in the CLI pre-check, once inside `integration.Sync` via `requireCleanWorkingState`).
