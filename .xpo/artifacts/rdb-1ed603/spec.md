# Spec: sqlLiteral backslash escaping

## What

`sqlLiteral` in `engine/engine.go:530` does not escape backslash characters in string values. The go-mysql-server SQL parser treats `\` as an escape character inside string literals (MySQL-compatible default), so backslashes in parameterized values are silently consumed, corrupting stored data.

## Why

The `?` parameter API promises transparent value binding. Any string passed as a parameter must round-trip exactly. Today, JSON data and any backslash-containing string is corrupted on storage.

## How

1. In `sqlLiteral`, `string` case: escape `\` → `\\` before the existing `'` → `''` escaping.
2. Add a regression test that inserts and round-trips strings containing: `\`, `\"`, `\\`, `\n`, `\t`, and a full JSON payload with nested escapes.

## Scope

- **In scope:** `sqlLiteral` in `engine/engine.go`, new test in `engine/engine_test.go`.
- **Out of scope:** The `client` package wire-protocol path (uses `database/sql` prepared statements, separate code path). Internal catalog/merge operations (operate at prolly tree level, not SQL).

## Acceptance criteria

- [ ] `sqlLiteral` escapes backslashes before single quotes
- [ ] Round-trip test: insert a string with backslashes via `?` param, read it back, assert exact match
- [ ] Test covers: bare `\`, escaped quote `\"`, double backslash `\\`, escape sequences `\n`/`\t`, full JSON payload
- [ ] All existing tests pass
