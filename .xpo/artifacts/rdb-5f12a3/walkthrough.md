# rdb-5f12a3 walkthrough: DDL and server options that weren't honored

## The problem

Several statements and options succeeded but didn't do what they said. go-mysql-server (GMS) decides what to do with many DDL statements by checking which optional interfaces the integrator's database and tables implement. When RepoDB didn't implement one, GMS did one of two things:
- fell back to behavior RepoDB can't keep, such as a session-local view registry;
- failed with a broken error string (`%!s(MISSING)`, `%!(EXTRA …)`).

The fix applies one rule to each item: honor it, or reject it clearly. User decisions:
- reject views, and file persisted views as rdb-5c1808;
- persist `TIME` precision;
- remove `--database`.

## Implementing interfaces to reject (`engine/unsupported.go`)

The new file holds interface implementations whose only job is to refuse:

- **`sql.ViewDatabase` on `database`.** Without it, GMS stores views in the session, so they disappear on the next session or reopen. `CreateView` now fails with `views are not supported (rdb-5c1808)`, `DropView` reports the view doesn't exist, and the getters return nothing.
- **`fulltext.Database` on `database`.** GMS's `getFulltextDatabase` returned `ErrCreateTableNotSupported.New()` with a missing format argument. Once the database implements the interface, GMS goes on to check the table, finds no FULLTEXT support, and returns its own clear `table does not support FULLTEXT indexes`. `CreateFulltextTableNames` is never reached; it returns an error only as a safeguard.
- **`sql.PrimaryKeyAlterableTable` on `*table`.** GMS's `buildAlterPK` formats `ErrNotPrimaryKeyAlterable` with an argument the format string doesn't use. `CreatePrimaryKey`/`DropPrimaryKey` now return a RepoDB message. That's the only place GMS checks this interface, so other `ALTER`s are unaffected.
- **`sql.IndexedTableCreator` on `database`.** This is GMS's `CREATE TABLE` path when the primary key has an index definition. It's the only place a primary-key prefix length like `PRIMARY KEY (v(5))` is visible, because `CreateTable`'s `PrimaryKeySchema` has only ordinals. `CreateIndexedTable` rejects prefixes, then delegates to `CreateTable`.

`rejectIndexPrefixes` is also called from `table.CreateIndex`, which serves `CREATE INDEX`, `ALTER TABLE ADD INDEX` and inline `INDEX` in `CREATE TABLE`. RepoDB always indexes whole values, so accepting a prefix would silently change what a `UNIQUE` index enforces.

## JSON keys (`engine/keycodec.go`)

GMS already rejects JSON in composite primary keys, in indexes and in `ALTER TABLE … MODIFY` of key or indexed columns. The only gap was a single-column `JSON PRIMARY KEY` in `CREATE TABLE`. That was accepted, and every insert then failed in `appendKeyColumn`. `CreateTable` now calls `validateKeyColumns(schema, PkOrdinals)`.

`keyable(typ)` lists the same types as `appendKeyColumn`'s switch. Two switches can drift apart, so `TestKeyableMatchesStoredTypes` checks every type `decodeType` can produce: each one is keyable except JSON, and each keyable type encodes its zero value. Adding a stored type without key support (or the reverse) fails that test.

I first planned extra checks in `CreateIndex` and `ModifyColumn`, but GMS rejects those cases before RepoDB sees them. I dropped them rather than keep code that can never run.

## TIME precision (`engine/catalog.go`)

`encodeSchema` recorded precision only for `sql.DatetimeType`, and `decodeType` always returned `types.Time` (precision 0). Precision is now recorded for `types.TimeType` in `encodeSchema` and `validateSchema`, and decoded with `types.CreateTimespanType(precision)`. Row values were already stored as microseconds, so the row format is unchanged. Older schemas have no TIME precision recorded and decode as `TIME(0)`, as before. Alpha has no real users, so no migration is needed.

## `--database` removal

`server.Config.DatabaseName` was given a default and then never used: the database name comes from the manifest's `DefaultDatabase`, which is shared across clones through sync. A per-server name would let clones disagree, so the field and the `repodb-server --database` flag are gone. `repodb sql --database` is a client flag (which database to connect to) and stays.

## Tests

- `engine/ddl_honored_test.go`, one test per item:
  - error messages are checked for content, and for the absence of `%!` formatting artifacts;
  - `TIME(p)` round-trips in journal and native-git modes across a reopen.
- `engine/keycodec_test.go`: `TestKeyableMatchesStoredTypes`.

## Docs

- `docs/sql.md`: types, keys, indexes and the DDL table describe the new behavior; `CREATE VIEW` cites rdb-5c1808.
- `docs/cli.md` and `docs/library.md`: no `--database` / `DatabaseName`.
- `docs/testing.md`: gap replaced with coverage.
