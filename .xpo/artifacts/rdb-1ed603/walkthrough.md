# Walkthrough: sqlLiteral backslash escaping

## What changed

`sqlLiteral` in `engine/engine.go` now escapes backslash characters (`\` → `\\`) in string parameters before the existing single-quote escaping. This is a two-line addition to the `string` case.

## Why

The `bind()` function interpolates `?` parameters into SQL text before handing it to the go-mysql-server parser. That parser follows MySQL's default behavior: `\` is an escape character inside string literals. Without escaping, every backslash in a parameterized string value was silently consumed by the parser — corrupting the stored data. This was especially destructive for JSON, where backslash escapes (`\"`, `\\`, `\n`) are pervasive.

## How the pieces fit

```
Session.Exec/Query(sql, args...)
  → bind(sql, args)        // replaces ? with literal values
    → sqlLiteral(arg)      // formats each arg as a SQL literal
      case string:         // ← the fix lives here
        escape \ → \\
        escape ' → ''
        wrap in single quotes
  → go-mysql-server parser // now sees doubled backslashes, stores one
```

The `[]byte` case was already safe (hex-encoded). The `client` package uses `database/sql` prepared statements which bypass `bind()` entirely.

## Test coverage

`TestStringParameterBackslashRoundTrip` inserts 7 strings through parameterized queries and asserts exact round-trip fidelity:

| Case | Input |
|------|-------|
| bare backslash | `a\b` |
| escaped quote | `hello \"world\"` |
| double backslash | `a\\b` |
| newline escape | `line1\nline2` |
| tab escape | `col1\tcol2` |
| JSON payload | `{"msg":"hello \"world\"","path":"C:\\Users\\test","nl":"a\nb"}` |
| single quote + backslash | `it's a \path` |

## Downstream impact

Consumers that previously worked around this bug (e.g. nate's `escapeForStorage`/`unescapeFromStorage`) should remove their workarounds — they would now double-escape.
