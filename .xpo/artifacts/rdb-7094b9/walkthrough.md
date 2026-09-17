# Walkthrough: Document CLI commands in README

## What changed

Originally scoped as implementing `commit` and `diff` CLI commands — rescoped after discovering all 11 commands are already implemented.

### CLI reference table
Added a new "CLI commands" section between the sync quickstart and "Embedded use" with a table covering all commands: `init`, `start`, `sql`, `status`, `diff`, `commit`, `enable`, `sync`, `conflicts`, `resolve`, `import-legacy`. Descriptions derived from the actual flag sets and behavior in `cmd/repodb/main.go`. Notable flags mentioned inline (e.g. `--addr`, `--persistence`, `--remote`).

### Roadmap
Collapsed the two M5 lines (partial + remaining) into one checked item since CLI commands, example applications, and tool-author validation are all complete.

## Why
The CLI was fully implemented but undocumented outside of the quickstart examples. Users had no single reference for what commands exist and what flags they accept.

## Decisions
- Placed the table after the sync quickstart (which introduces several commands in context) and before "Embedded use" — this keeps the flow from quickstart → full reference → programmatic API.
- Used a table rather than a subsection-per-command since each command is a one-liner. The quickstart already shows usage examples.
- Omitted `snapshot` (deprecated/dead command that returns an error).
