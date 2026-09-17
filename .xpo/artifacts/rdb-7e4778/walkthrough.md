# Walkthrough: Update README for remote default and dirty-journal prompt

## What changed

README.md updated to reflect two recent features:

1. **`--remote` defaults to configured remote** (rdb-bc2820) — The `conflicts` and `resolve` examples no longer pass `--remote origin`, and a new note explains that all sync commands default to the remote set by `enable`. The CLI table descriptions for `sync` and `conflicts` were updated to remove the `--remote` parameter callout.

2. **Dirty-journal checkpoint prompt** (rdb-3c8ef9) — The `sync` description now mentions that it prompts to checkpoint uncommitted journal changes before proceeding. The CLI table entry for `sync` reflects this too.

## Key decisions

- Kept the `--remote` flag documented on `enable` (that's where you set it) and mentioned "pass `--remote` only to override" rather than listing it on every command.
- Described the checkpoint prompt briefly inline rather than adding a separate section — it's a single interactive prompt, not a workflow change.
