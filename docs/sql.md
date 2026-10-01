# SQL reference

RepoDB runs SQL through [go-mysql-server](https://github.com/dolthub/go-mysql-server) and speaks the MySQL dialect. The embedded library and the MySQL wire server share one engine, so everything here applies to both. This page lists what RepoDB stores and enforces. Anything not listed should be treated as unsupported.

Where a feature is planned, the backlog issue is named in parentheses.

## Database

There is one database, `repodb`, plus the read-only `information_schema`. Every session starts with `repodb` selected. `USE` of another database fails, and `CREATE DATABASE` is rejected.

## Tables

Every table needs an explicit `PRIMARY KEY`, which may be composite. A primary-key column can never be NULL. `CREATE TABLE` without one fails with `RepoDB requires an explicit PRIMARY KEY`.

`CREATE TABLE` supports:
- `NOT NULL`;
- `DEFAULT` literals and expressions, e.g. `DEFAULT (1 + 1)`, plus the `DEFAULT` keyword in `INSERT … VALUES`;
- `CHECK` constraints, named or unnamed, including `NOT ENFORCED`;
- `UNIQUE` and `INDEX`/`KEY` clauses, inline or as table elements;
- a collation per column.

A table-level `COLLATE` or `CHARSET` is ignored: only column collations are stored.

### Column types

| Type | Stored parameters |
| --- | --- |
| `TINYINT`, `SMALLINT`, `MEDIUMINT`, `INT`, `BIGINT`, with or without `UNSIGNED` (`BOOL` is `TINYINT`) | none |
| `FLOAT`, `DOUBLE` | none |
| `DECIMAL(p, s)` / `NUMERIC` | precision up to 65 and scale up to 30; bare `DECIMAL` is `DECIMAL(10, 0)`. Values that overflow are rejected; excess scale is rounded. |
| `CHAR(n)`, `VARCHAR(n)`, `TEXT` | length (default 65535) and collation |
| `BINARY(n)`, `VARBINARY(n)`, `BLOB` | length (default 65535) |
| `DATE`, `DATETIME(p)`, `TIMESTAMP(p)` | fractional-second precision 0–6. Values are stored as UTC microseconds. |
| `TIME(p)` | fractional-second precision 0–6. Values are stored as microseconds. |
| `ENUM(…)` | the value list and collation. Values sort by definition order. |
| `JSON` | stored as canonical JSON text; invalid JSON is rejected |

These types are not supported, and the error is `unsupported SQL type <type>`:
- `YEAR`, `SET`, `BIT`;
- spatial types and `VECTOR`.

### Keys and indexes

- **Key columns.** Primary keys, `UNIQUE` constraints and secondary indexes can use any type above except `JSON`. `FLOAT`/`DOUBLE` keys reject NaN. DDL that would put a `JSON` column in a key is rejected.
- **Uniqueness.** `UNIQUE` indexes allow multiple NULLs, as in MySQL. Uniqueness and key lookups follow the column collation: under a `_ci` collation, `'alice'` and `'Alice'` are the same key. `CHAR` keys ignore trailing spaces.
- **Index DDL.** `CREATE [UNIQUE] INDEX`, `ALTER TABLE … ADD [UNIQUE] INDEX`, `DROP INDEX` and `RENAME INDEX` are supported. Adding a `UNIQUE` index over existing duplicates is rejected. Index names are case-sensitive.
- **Not supported.** Prefix lengths such as `(v(5))` are rejected with `index prefix lengths are not supported`, in indexes and primary keys. `FULLTEXT` indexes are rejected with `table does not support FULLTEXT indexes`. `SPATIAL` indexes are not supported.

Index-using queries:
- **Point lookups** use the primary key or a secondary index.
- **Ranges** (`<`, `<=`, `>`, `>=`, `BETWEEN`, `IN`, `IS [NOT] NULL`) use an index when the predicate fixes a prefix of the index columns and ranges over at most one following column. Other shapes still return correct results, from a full scan with a filter.
- **Ascending order.** `ORDER BY <index columns> [ASC] … LIMIT n` is served in index order without sorting, and stops after `n` rows.
- **Descending order.** `ORDER BY … DESC` sorts the matching rows (rdb-acd36d).
- **Secondary lookups** fetch each row by primary key; indexes are never covering.

### Changing tables

| Statement | Supported | Notes |
| --- | --- | --- |
| `ALTER TABLE … ADD COLUMN` (`FIRST`/`AFTER`) | yes | A `NOT NULL` column needs a `DEFAULT` when the table has rows. It cannot add a primary-key column. |
| `ALTER TABLE … DROP COLUMN` | yes | Not the only primary-key column, and not a column used by an index. Dropping part of a composite key re-keys the rows and fails on duplicates. |
| `ALTER TABLE … MODIFY` / `CHANGE COLUMN`, `RENAME COLUMN` | yes | Converts existing values and fails on values that don't fit, on NULLs under a new `NOT NULL`, and on new unique-key duplicates. Changing a key column's type re-keys the rows. |
| `ALTER TABLE … ADD/DROP CHECK` / `CONSTRAINT` | yes | Adding a check that existing rows violate is rejected. |
| `DROP TABLE` | yes | |
| `ALTER TABLE … ADD/DROP PRIMARY KEY` | no | Rejected: every RepoDB table keeps the primary key it was created with. |
| `RENAME TABLE`, `ALTER TABLE … RENAME TO` | no | |
| `TRUNCATE TABLE` | no | Use `DELETE FROM t`. |
| `CREATE TEMPORARY TABLE` | no | |
| `AUTO_INCREMENT` | no (rdb-a2d28e) | Rejected with `column <c> uses AUTO_INCREMENT or a generated column, which RepoDB does not support`. |
| Generated columns | no | Same error as `AUTO_INCREMENT`. |
| `FOREIGN KEY` | no (rdb-48af2f) | |
| Triggers, stored procedures | no | |
| `CREATE VIEW` | no (rdb-5c1808) | Rejected with `views are not supported`. |

## Reading and writing rows

`SELECT` supports the go-mysql-server query surface over RepoDB tables: joins, subqueries, aggregates, window functions and built-in functions, including the JSON functions (`JSON_EXTRACT`, `JSON_OBJECT`, …). Queries over `information_schema` also work.

`INSERT` (single-row, multi-row and `INSERT … SELECT`), `INSERT IGNORE`, `INSERT … ON DUPLICATE KEY UPDATE`, `UPDATE` (including of primary-key values) and `DELETE` are supported. `REPLACE` is not supported.

A duplicate primary key is reported as a unique-key error. The error shows the encoded key, not the column values.

`SHOW TABLES`, `SHOW DATABASES`, `SHOW CREATE TABLE`, `SHOW INDEX`, `DESCRIBE <table>` and `EXPLAIN` are available. RepoDB serves a plain `EXPLAIN <statement>` (or `DESCRIBE`/`DESC <statement>`) as `EXPLAIN PLAN`: one `plan` column showing the operator tree, e.g. whether `IndexedTableAccess` is used. `EXPLAIN FORMAT=…` and `EXPLAIN ANALYZE` are passed through to go-mysql-server unchanged. Over the wire, prepared statements are not rewritten.

## Transactions

- **Autocommit and explicit transactions.** Each autocommit statement is its own transaction. `BEGIN` / `START TRANSACTION`, `COMMIT` and `ROLLBACK` group statements; `ROLLBACK` discards all of the transaction's changes.
- **Snapshot isolation.** A transaction reads one snapshot, taken at its first table access, plus its own writes. Other sessions' commits become visible at the next transaction.
- **Commit conflicts.** A write transaction commits only if nothing else committed since its snapshot was taken, even if the other commit changed different rows. Otherwise `COMMIT` fails with `RepoDB data head changed` (`repository.ErrConflict`), and the application should retry the whole transaction. A failed `COMMIT` ends the transaction, as in MySQL: the session is back in autocommit mode on the current data, so the retry starts fresh. RepoDB never replays statements itself. Per-row conflict detection, automatic retry of single statements and MySQL deadlock error codes are tracked in rdb-df092b.
- **Failed statements.** A statement that fails inside an explicit transaction undoes only its own changes; the transaction stays open.
- **DDL.** DDL commits implicitly, as in MySQL: a `CREATE TABLE` inside a transaction survives `ROLLBACK`.
- **Savepoints** are not supported (`RepoDB does not support savepoints`).
- **Read-only transactions.** `START TRANSACTION READ ONLY` allows reads. Any `INSERT`, `UPDATE` or `DELETE` inside it fails with `cannot execute statement in a READ ONLY transaction` (MySQL error 1792), and the transaction stays open.
- **Uncertain commit outcomes.** If a commit's outcome is uncertain (for example, the process failed while updating the Git ref), the error carries the candidate commit ID. `SELECT repodb_recover_commit('<candidate>')` returns `committed`, `rejected` or `unknown`. Use it instead of re-running the transaction. See [library.md](library.md#commit-outcomes).

## Functions added by RepoDB

| Function | Returns |
| --- | --- |
| `repodb_recover_commit(candidate)` | `committed`, `rejected` or `unknown` for a commit candidate ID reported in a commit error |

## Server access

The server listens on `127.0.0.1:3306` by default and accepts any user name and password. There is no authentication, TLS or privilege system (rdb-5720c1, rdb-8d63a3). Driver and ORM compatibility is untested beyond `go-sql-driver/mysql` (rdb-686c51, rdb-65b203, rdb-8b1539).

Historical queries (`AS OF` a data commit) are not supported (rdb-71f167).
