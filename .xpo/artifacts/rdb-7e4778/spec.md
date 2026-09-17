# Spec: Update README for remote default and dirty-journal prompt

## What
Update README.md to document two recent behavior changes.

## Why
The README examples and CLI table are out of date after rdb-bc2820 and rdb-3c8ef9.

## How

1. **Drop `--remote origin` from `conflicts` and `resolve` examples** (line ~58-59) — after `enable --remote origin`, the remote is remembered.
2. **Add a note** after the example block explaining that `--remote` defaults to the remote configured by `enable`, so it only needs to be passed to override.
3. **CLI table updates:**
   - `sync` — mention it prompts to checkpoint uncommitted journal changes.
   - `conflicts` and `resolve` — note `--remote` defaults to configured remote.
4. **Sync paragraph** (~line 55) — mention the checkpoint prompt behavior briefly.

## Acceptance Criteria
- [ ] `conflicts` and `resolve` examples omit `--remote origin`
- [ ] A note explains `--remote` defaults to the configured remote
- [ ] CLI table descriptions updated for sync, conflicts, and resolve
- [ ] Dirty-journal checkpoint prompt mentioned near sync documentation
