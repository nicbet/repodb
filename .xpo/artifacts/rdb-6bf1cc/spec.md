# Plain EXPLAIN fails over the MySQL protocol

## What

`repodb sql 'EXPLAIN SELECT …'`, and any MySQL client sending a plain `EXPLAIN`/`DESCRIBE <statement>` to `repodb start`, fails with `strconv.ParseUint: parsing "NULL": invalid syntax`.

## Why

Upstream go-mysql-server's tabular EXPLAIN is an explicit placeholder (`rowexec/show.go` `buildDescribeQuery`). It warns "EXPLAIN Output is currently a placeholder; use EXPLAIN PLAN for old behavior" and returns a dummy row that puts the *string* `"NULL"` in every column. That includes `rows`, declared `Uint64` NOT NULL, so encoding the row for the wire fails. Embedded use never serializes it, but only ever receives the useless placeholder. The engine's own tests already use `EXPLAIN PLAN`.

## How

- `engine.NormalizeExplain(query string) string`: parse the first statement with the Vitess parser. If it is a `*sqlparser.Explain` with no `Plan`, no `Analyze` and an empty `ExplainFormat` (a plain `EXPLAIN`/`DESCRIBE`/`DESC <statement>`), replace the leading keyword (after whitespace and comments) with `EXPLAIN PLAN` and re-parse to confirm. Otherwise return the query unchanged.
  - Untouched: `DESCRIBE <table>` (parsed as `Show`), `EXPLAIN PLAN`, `EXPLAIN FORMAT=…`, `EXPLAIN ANALYZE`, and anything that fails to parse.
  - Only the leading keyword changes, so multi-statement text keeps its remainder.
- **Server:** a go-mysql-server `Interceptor`, registered through `InterceptorChain.Option()` in `server.New`. It applies `NormalizeExplain` in `Query` and `MultiQuery`. `Prepare`, `StmtExecute` and `ParsedQuery` pass through; explaining prepared statements is out of scope.
- **Embedded:** `Session.Query`/`Exec` and transaction `Query`/`Exec` apply `NormalizeExplain` before handing the statement to go-mysql-server.

Result: plain EXPLAIN returns the real plan tree (one `plan` column), as `EXPLAIN PLAN` does, over the wire and embedded.

## Decisions

- **Return the plan instead of a MySQL-style table.** The tabular output upstream is a placeholder with no information, and over the wire it cannot be sent at all. A plan tree is strictly more useful. Clients expecting MySQL's column layout get a different shape, but today they get an error.
- **Parser-based detection**, not string matching, so `DESCRIBE <table>` and explicit formats are never misread.

## Acceptance criteria

- `NormalizeExplain` unit tests: EXPLAIN/DESCRIBE/DESC of SELECT/UPDATE (any case, leading comments, multi-statement) are rewritten; `DESCRIBE t`, `EXPLAIN t`, `EXPLAIN PLAN`, `FORMAT=TREE`, `ANALYZE` and unparsable text are unchanged.
- The server test runs `EXPLAIN SELECT …` through the MySQL client and gets plan rows containing the table access. `DESCRIBE <table>` still returns column rows.
- The embedded engine returns the plan for a plain EXPLAIN.
- `repodb sql 'EXPLAIN …'` works end to end with the binary.
- `make test` and `make lint` pass.
