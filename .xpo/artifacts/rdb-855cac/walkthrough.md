# Walkthrough: upgrade go-mysql-server to latest main

## What changed and why

RepoDB pinned go-mysql-server (GMS) v0.20.0 from May 2025, the last tag GMS published. It dragged in old indirect dependencies with advisories (grpc, protobuf, x/text, logrus). GMS development continues on `main`, which Dolt consumes through pseudo-versions, so we now do the same: `v0.20.1-0.20260925233942-d8e4c765db15`.

Consequences, each handled below: a Go 1.26.2 floor, API breaks, a cgo/ICU regex dependency, a new DECIMAL representation, and a non-idempotent server `Close`.

## Go directive

GMS main declares `go 1.26.2`, so ours does too (it supersedes rdb-d3c729's 1.25; the user approved). x/sys and x/term keep the minimum versions from rdb-d3c729, because nothing needs newer.

## The `gms_pure_go` build tag

Since September 2025, `go-icu-regex` binds ICU4C through cgo. Previously it ran ICU compiled to WebAssembly on wazero, which is pure Go. Without intervention, every RepoDB build would need a C++ toolchain and ICU headers, and the Windows cross-compile would break.

GMS offers `-tags gms_pure_go`, which swaps in Go's `regexp` for SQL `REGEXP`/`RLIKE`/`REGEXP_*`. The trade-off is RE2 syntax instead of ICU's. RepoDB doesn't use SQL regex functions itself.

Build tags can't be declared in `go.mod`, so:
- `Makefile` has `export GOFLAGS += -tags=gms_pure_go`, so every target gets it.
- The README tells library users and `go install` users to pass the tag, or else install ICU4C and use cgo. We deliberately don't add a guard that forces the tag, because users with ICU may want MySQL-exact regexes.
- Running `go test` directly (outside make) needs the tag too, or `GOFLAGS` set.

## API adaptations (engine/)

Mostly mechanical:
- `Schema`, `PrimaryKeySchema`, `ColumnExpressionTypes`, and `Expression.Type/IsNullable/WithChildren` gained a `*sql.Context` parameter.
- `sql.Function1.Fn` takes a context.
- `Type.Convert` returns `sql.ConvertInRange` (InRange/Underflow/Overflow) instead of a bool.
- `sql.Index.CoversColumns` is new. It only feeds the cost model: "covering" means the planner can skip fetching full rows. RepoDB lookups always return full rows, so both indexes return `false`.
- `types.JsonToMySqlString` needs a context. We pass `context.Background()` inside `rawValue`, because the context is only used by lazily loaded JSON (Dolt's storage), and threading one through ~40 encode call sites would be churn with no effect.

## DECIMAL: new in-memory type, same bytes on disk

GMS switched DECIMAL values from shopspring `decimal.Decimal` to cockroachdb `*apd.Decimal`.

**Storage must not change.** Rows, primary keys and secondary-index keys store decimals as text. The store is content-addressed, and lookups compare encoded key bytes. The old text was shopspring's `String()`, which trims trailing zeros (`0.1`, `12345678.5`). If we had switched to apd's formatting (`0.10`), existing index entries would no longer match freshly encoded lookup keys, and identical data would hash differently.

So `rawValue` converts `*apd.Decimal` → shopspring → `String()`, which keeps the bytes identical. Decoding parses with `apd.NewFromString` and runs it through the column type's `Convert`.

This was verified with real binaries: the old binary wrote DECIMAL PK plus UNIQUE-index data through both native-git and journal persistence. The new binary found every row by PK and by index, and rejected duplicate PK and unique values.

**Visible API change:** `Session.Query` passes GMS values through untouched, so embedded users now receive `*apd.Decimal`, printed at column scale (`0.10`). That is what MySQL returns, and `TestDecimalRoundTrip` now expects it. SQL parameter formatting in `engine.go` accepts both decimal types.

## Idempotent `server.Close`

GMS's `Server.Close` now closes an internal channel unconditionally, so a second call panics. `TestMySQLWireRoundTrip` closes explicitly and again in `t.Cleanup`, and `Serve` can close on context cancellation. `server.Server.Close` now wraps the real close in a `sync.Once` and returns the first result.

## Security bumps beyond GMS

GMS main still required versions with open advisories, so RepoDB raises them directly (MVS picks the max):
- grpc **v1.83.2**. The GO-2026-6443 fix landed per release branch (1.82.2 and 1.83.2), and v1.84.0 is still affected, so "latest" would have been wrong.
- x/text v0.41.0 (≥ v0.39.0 needed for GO-2026-5970).
- otel v1.44.0, pulled in by grpc (fixes GO-2026-5158).

govulncheck reports no vulnerabilities in any module.

## Found along the way (pre-existing on main)

- rdb-3745e6: `repodb commit` drops journal row edits.
- rdb-e472aa: journal writes skip secondary indexes when the table already has snapshot rows.
- rdb-6bf1cc: `EXPLAIN` via `repodb sql` fails on a NULL column.
