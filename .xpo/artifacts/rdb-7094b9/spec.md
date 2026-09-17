# Spec: Document CLI commands in README

## What
Add a CLI reference section to README.md listing all 11 `repodb` subcommands, and mark M5 as complete in the roadmap.

## Why
The CLI is fully implemented but the README only shows a few commands in the quickstart. Users have no single reference for what commands exist. M5 is complete and the roadmap should reflect that.

## How

### 1. Add CLI reference section
Insert a "CLI commands" section (after Quickstart, before Embedded use) with a concise table or list of all commands and one-line descriptions. Pull descriptions from the actual usage/help text in `cmd/repodb/main.go`.

### 2. Update roadmap
Change M5 from partial to fully complete. Collapse the two M5 lines back into one checked item.

## Acceptance criteria
- [ ] All 11 commands listed with accurate one-line descriptions
- [ ] M5 marked as done in roadmap
- [ ] No duplication with quickstart examples (reference complements, doesn't repeat)
