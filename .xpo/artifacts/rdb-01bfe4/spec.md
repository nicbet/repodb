# Spec: Update README to reflect shipped features

## What
Update README.md to remove outdated "not yet supported" claims and reflect features that have shipped since the last README update.

## Why
The README is the project's public face. It currently tells readers that secondary indexes, ALTER TABLE, and broader types are missing — all of which have shipped. This misleads potential users and contributors.

## How

### 1. "Current SQL scope" paragraph (~line 195-197)
Replace the sentence listing unsupported features. The new text should mention:
- Secondary indexes (unique and non-unique) are supported
- ALTER TABLE (non-destructive schema migrations) is supported
- CHECK constraints and DEFAULT values are supported
- Expanded type support: ENUM, DECIMAL/NUMERIC, JSON, DATE/TIME/DATETIME/TIMESTAMP
- Collation-aware string comparisons
- Auto-increment and foreign keys remain unsupported

### 2. Roadmap (~lines 209-215)
- Mark M5/M6 items that have shipped (secondary indexes, broader SQL compatibility) as partially complete or restructure to show what's done vs. remaining.

### 3. No other changes
- Quickstart, embedded example, architecture diagram, performance table, and installation sections are still accurate — leave them alone.

## Acceptance criteria
- [ ] No claim of "not yet supported" for features that exist
- [ ] Roadmap reflects current progress
- [ ] No unrelated changes to README
