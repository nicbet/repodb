# rdb-5f12a3: DDL and server options accepted but not honored

Each item either works as written or fails with a clear error. User decisions (2026-10-01): reject views and file a feature (rdb-5c1808); persist TIME precision; remove `--database`/`DatabaseName`; keep this as one issue.

The explicit rejections live in `engine/unsupported.go`. RepoDB implements some go-mysql-server interfaces there only to refuse the statements they serve.

## 1. Views → reject

**Was:** `database` didn't implement `sql.ViewDatabase`, so go-mysql-server kept views in a session-local registry, and they were lost.

**Now:** `database` implements `sql.ViewDatabase`:
- `CreateView` returns `views are not supported (rdb-5c1808)`;
- `DropView` returns `sql.ErrViewDoesNotExist`;
- `GetViewDefinition` returns not found;
- `AllViews` returns nil.

**Test:** `TestCreateViewIsRejected`.

## 2. JSON key columns → reject

**Was:** a single-column `JSON PRIMARY KEY` was accepted, and every insert then failed.

**Found:** go-mysql-server already rejects every other JSON key case with `JSON column '<c>' supports indexing only via generated columns…`:
- composite primary keys;
- `CREATE INDEX` / `ADD INDEX`;
- `ALTER TABLE … MODIFY` of a primary-key or indexed column.

**Now:** `CreateTable` calls `validateKeyColumns(schema, PkOrdinals)` (`engine/keycodec.go`), which uses `keyable(typ)` and returns `column <c> of type <t> cannot be part of a key`. No checks were added to `CreateIndex` or `ModifyColumn`: go-mysql-server stops those cases first, so the checks could never run. `keyable` mirrors `appendKeyColumn`'s cases. `TestKeyableMatchesStoredTypes` checks that every type `decodeType` stores is keyable except JSON, and that each keyable type encodes its zero value.

**Test:** `TestJSONKeyColumnsAreRejected` covers RepoDB's error and go-mysql-server's cases, and that a JSON non-key column still works.

## 3. Index prefix lengths → reject

**Now:** `rejectIndexPrefixes` returns `index prefix lengths are not supported (<c>(<n>))`. Called from:
- `CreateIndex`: `CREATE [UNIQUE] INDEX`, `ALTER TABLE ADD INDEX`, and inline `INDEX` in `CREATE TABLE`;
- `CreateIndexedTable`: `database` now implements `sql.IndexedTableCreator`, the path go-mysql-server uses for `CREATE TABLE` with a primary-key index definition such as `PRIMARY KEY (v(5))`. After the check it delegates to `CreateTable`.

**Test:** `TestIndexPrefixLengthsAreRejected` covers five forms, and that a full-column index still works.

## 4. TIME(n) → persist precision

**Now:**
- `encodeSchema` and `validateSchema` record `types.TimeType.Precision()`.
- `decodeType` returns `types.CreateTimespanType(precision)`.

The row format is unchanged (microseconds). Schemas without recorded precision decode as `TIME(0)`, as before.

**Test:** `TestTimePrecisionIsPersisted`, journal and native-git:
- `TIME(3)` appears in `SHOW CREATE TABLE`;
- `MODIFY y TIME(6)` persists;
- both survive a reopen;
- `'12:34:56.789'` reads back exactly.

## 5. `repodb-server --database` / `server.Config.DatabaseName` → removed

Removed the flag from `cmd/repodb-server`, and the field and its defaulting from `server.Config`. `server_test.go` was updated. `repodb sql --database` (client side) stays.

## 6. FULLTEXT → clear error

**Found:** once the database implements `fulltext.Database`, go-mysql-server checks the table next and returns its own `table does not support FULLTEXT indexes` (`sql.ErrFullTextNotSupported`). That message is clear, so RepoDB uses it instead of a custom one. `database.CreateFulltextTableNames` exists only to satisfy the interface; it returns `FULLTEXT indexes are not supported` and is never reached.

**Test:** `TestFulltextIndexesAreRejected` covers:
- `CREATE FULLTEXT INDEX`;
- `ALTER TABLE ADD FULLTEXT`;
- inline `FULLTEXT KEY` in `CREATE TABLE`, after which the table doesn't exist.

## 7. `ADD`/`DROP PRIMARY KEY` → clear error

**Now:** `*table` implements `sql.PrimaryKeyAlterableTable`. `CreatePrimaryKey` and `DropPrimaryKey` return `changing a table's primary key is not supported; RepoDB tables always have one`. go-mysql-server checks this interface only in `buildAlterPK`.

**Test:** `TestPrimaryKeyChangesAreRejected` covers both statements, and that the schema is unchanged.

## Docs

- `docs/sql.md`:
  - `TIME(p)` row;
  - key-column, prefix and FULLTEXT notes;
  - `ADD`/`DROP PRIMARY KEY` and `CREATE VIEW` rows (citing rdb-5c1808).
- `docs/cli.md` and `docs/library.md`: `--database` and `DatabaseName` removed; the server exposes the manifest's single database.
- `docs/testing.md`: gap removed; "Unsupported DDL" coverage added.
- No `docs/` page cites rdb-5f12a3.

## Acceptance

Each item has a test proving the statement is honored or rejected with a clear error. `make test` and `make lint` pass. Docs describe the new behavior.
