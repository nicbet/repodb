# Walkthrough: Update README to reflect shipped features

## What changed

Two sections of `README.md` were updated to match the current state of the project.

### SQL scope paragraph (line 195)

The old text claimed secondary indexes, `ALTER TABLE`, and broader types were unsupported. Replaced with an accurate inventory of current capabilities:

- DDL/DML including `ALTER TABLE`
- Secondary indexes (unique and non-unique)
- `CHECK` constraints and `DEFAULT` values
- Collation-aware string comparisons
- Full type list: integers, floats, `TEXT`, `BLOB`, `BOOL`, `ENUM`, `DECIMAL`/`NUMERIC`, `JSON`, `DATE`, `TIME`, `DATETIME`, `TIMESTAMP`

Only auto-increment and foreign keys remain listed as unsupported.

### Roadmap (line 213)

Split M5 and M6 into completed and remaining items:

- **M5 partial (done):** Example applications and tool-author validation — satisfied by Nate's real-world usage.
- **M5 remaining:** CLI completion (`commit`, `diff`).
- **M6 partial (done):** Secondary indexes, ALTER TABLE, expanded types/constraints, collation.
- **M6 remaining:** Auto-increment, foreign keys, operational hardening.

## Why

The README is the project's public entry point. Outdated "not supported" claims mislead users and undersell the project's current capabilities.

## Decisions

No judgment calls were needed — all changes are factual corrections based on shipped commits.
