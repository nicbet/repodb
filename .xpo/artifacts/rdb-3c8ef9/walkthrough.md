# Walkthrough: repodb sync — prompt to checkpoint when journal is dirty

## What was built

When `repodb sync` is run interactively (TTY on stdin) and the journal has uncheckpointed changes, the CLI now prompts the user to auto-checkpoint before proceeding. Previously, sync would hard-fail with an error message telling the user to run `repodb commit -m ...` manually.

## How the pieces fit together

The change lives entirely in the CLI layer (`cmd/repodb/main.go`). The integration package (`integration.Sync`) is untouched — it remains a pure library function with no interactive I/O.

### New function: `promptCheckpointIfDirty`

Added at `cmd/repodb/main.go:248`. This function:

1. Opens the repository and working state at the given path
2. Returns early (nil) if no working state exists or it's clean
3. If dirty: prints a prompt to stderr with the generation number
4. Reads one line from stdin via `bufio.NewReader`
5. On `y`/`Y`: calls `working.Checkpoint(ctx, "checkpoint before sync")` and prints the commit hash to stderr
6. On anything else (including EOF from a broken pipe): returns the same `ErrWorkingDirty` error that `requireCleanWorkingState` would produce

### Wiring in the sync handler

In the `sync` case (~line 157), before calling `integration.Sync`, a TTY check gates the prompt:

```go
if term.IsTerminal(int(os.Stdin.Fd())) {
    if err := promptCheckpointIfDirty(ctx, *repoPath); err != nil {
        return err
    }
}
```

When there's no TTY (piped input, cron, CI), this block is skipped entirely, and `integration.Sync` handles the dirty state with its existing `requireCleanWorkingState` error — zero behavior change for non-interactive callers.

## Key decisions

- **Prompt on stderr, not stdout** — keeps stdout clean for machine-parseable sync output. Matches how Git prompts work.
- **Default No (`[y/N]`)** — checkpoint is a write operation; safe default is to abort. User must actively opt in.
- **Fixed commit message `"checkpoint before sync"`** — not worth prompting for a message on top of the y/n. Users can see what was checkpointed via `repodb status` or git log.
- **`golang.org/x/term` for TTY detection** — stdlib-adjacent package, consistent with the existing `golang.org/x/sys` dependency. Avoids pulling in third-party `mattn/go-isatty`.
- **Double open of repository** — `promptCheckpointIfDirty` opens the repo to check status, then `integration.Sync` opens it again. The status check is cheap (journal stat + replay from cache), and this keeps the integration API unchanged.

## Non-obvious details

The `requireCleanWorkingState` inside `integration.Sync` is called at three points (before the retry loop, at the top of each iteration, and after fetch). After a successful auto-checkpoint from the prompt, the first check passes. The mid-sync checks guard against a different scenario: another process dirtying the journal during the sync window. This is the same race window that existed with manual `repodb commit && repodb sync`.
