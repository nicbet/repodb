# Walkthrough: plain EXPLAIN returns the plan

## The bug

`repodb sql 'EXPLAIN SELECT …'` failed with `strconv.ParseUint: parsing "NULL": invalid syntax`, and so did any MySQL client sending a plain `EXPLAIN` or `DESCRIBE <statement>` to `repodb start`.

## Root cause (upstream)

go-mysql-server's tabular EXPLAIN (`sql/rowexec/show.go`, `buildDescribeQuery`) is an explicit placeholder. It warns "EXPLAIN Output is currently a placeholder; use EXPLAIN PLAN for old behavior" and returns one dummy row with the **string** `"NULL"` in every column. Its schema (`plan.DescribeSchema`) declares `id` and `rows` as `Uint64` NOT NULL, so encoding the row for the MySQL protocol fails when it parses `"NULL"` as a number.

Embedded sessions never serialize rows, so they didn't fail, but they only ever received the placeholder. RepoDB's own tests had already moved to `EXPLAIN PLAN`.

## The fix

**`engine.NormalizeExplain(query)`** (`engine/explain.go`) rewrites a plain EXPLAIN, DESCRIBE or DESC of a statement to `EXPLAIN PLAN`, and leaves everything else untouched:
- **Cheap gate first.** `leadingKeyword` skips whitespace and `/* */`, `--` and `#` comments and reads the first word. Only EXPLAIN, DESCRIBE and DESC go further, so ordinary queries (the hot path) are never parsed twice.
- **Parser decides.** `sqlparser.ParseOne` must yield `*sqlparser.Explain` with no `Plan`, no `Analyze` and an empty `ExplainFormat`. `DESCRIBE t` and `EXPLAIN t` parse as `*Show` (column listings) and are left alone, as are `EXPLAIN PLAN`, `FORMAT=…` and `ANALYZE`.
- **Minimal edit.** Only the leading keyword is replaced, so comments and multi-statement remainders are preserved. The result is re-parsed to confirm it is `EXPLAIN PLAN`; otherwise the original is returned.

**Where it applies:**
- Embedded: `Session.Query`, which every `Exec` and `Tx` path funnels through.
- Server: `server/explain.go` implements go-mysql-server's `Interceptor`, registered in `server.New` via `InterceptorChain.Option()`. `Query` and `MultiQuery` normalize, while `Prepare`, `StmtExecute` and `ParsedQuery` pass through. Explaining prepared statements is out of scope; go-mysql-server's chain never calls `ParsedQuery`.

## Visible behavior

A plain EXPLAIN now returns one `plan` column with the plan tree, like `EXPLAIN PLAN`, instead of MySQL's tabular layout. That layout was a placeholder upstream and couldn't be sent over the wire at all.

## Tests

- `engine/explain_test.go` `TestNormalizeExplain`: 17 cases covering rewrites (case, comments, DESC and DESCRIBE, UPDATE, multi-statement) and non-rewrites (`DESCRIBE t`, `EXPLAIN t`, explicit formats, `ANALYZE`, string literals, garbage, empty, unterminated comment).
- `TestPlainExplainReturnsPlan`: embedded EXPLAIN and DESCRIBE of a query return plan rows; `DESCRIBE t` returns column rows.
- `server/server_test.go` `TestMySQLWireRoundTrip`: EXPLAIN over the MySQL protocol returns the plan, and DESCRIBE returns columns. With the interceptor removed it fails with the original ParseUint error.
