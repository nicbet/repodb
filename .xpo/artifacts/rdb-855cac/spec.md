# Upgrade go-mysql-server to latest main

## What

Move `github.com/dolthub/go-mysql-server` from v0.20.0 (May 2025) to latest `main` (`v0.20.1-0.20260925233942-d8e4c765db15`), adapt RepoDB to its API changes, build with the `gms_pure_go` tag, and raise the remaining vulnerable indirect dependencies to their minimum fixed versions.

## Why

v0.20.0 pulls in old indirect dependencies that govulncheck flags (grpc v1.53.0, protobuf v1.28.1, x/text v0.6.0, logrus v1.8.1). RepoDB does not reach the vulnerable code today, but the pin is 16 months old and only gets harder to move.

GMS publishes no tags after v0.20.0; Dolt consumes `main` via pseudo-versions, so we do the same.

## How

1. `go get github.com/dolthub/go-mysql-server@main`, `go mod tidy`. The go directive becomes 1.26.2 (GMS's floor). x/sys and x/term stay at the rdb-d3c729 minimums.
2. Fix API breaks in `engine/`:
   - `*sql.Context` parameter added to `Schema`, `PrimaryKeySchema`, `ColumnExpressionTypes`, and `sql.Expression`'s `Type`/`IsNullable`/`WithChildren`.
   - `sql.Function1.Fn` is now `func(*sql.Context, sql.Expression)`.
   - `Type.Convert` returns `sql.ConvertInRange` (InRange/Underflow/Overflow) instead of bool: compare against `sql.InRange`.
   - `sql.Index` gained `CoversColumns([]string) bool`: return false (see Decisions).
   - `types.JsonToMySqlString` takes a context: pass `context.Background()` in `rawValue`.
   - DECIMAL values are now `*apd.Decimal` (cockroachdb/apd) instead of shopspring `decimal.Decimal`: see Decisions.
3. `server.Server.Close` becomes idempotent (`sync.Once`). GMS's `Server.Close` now panics on a second call (it closes a channel twice), and RepoDB callers, including tests and `Serve`, may close twice.
4. Build tag: the Makefile exports `GOFLAGS += -tags=gms_pure_go`, so all targets use it.
5. README: Go 1.26 or newer; explain the tag for `go get` / `go install` / `go test -race` users.
6. Security bumps above what GMS requires: grpc v1.83.2 (GO-2026-6443/6441/6348/6061; note v1.84.0 is still affected by 6443), and x/text v0.41.0 (GO-2026-5970; ≥ v0.39.0 needed, v0.41.0 is what the resulting graph selects). otel v1.44.0 comes in via grpc and fixes GO-2026-5158.

## Decisions

- **`gms_pure_go` over cgo + ICU4C.** Since 2025-09, go-icu-regex links ICU4C through cgo. Requiring a C++ toolchain and ICU would end RepoDB's "just `go get` it" story and break the Windows cross-compile. With the tag, SQL `REGEXP` functions use Go's `regexp` (RE2 syntax). RepoDB itself uses no SQL regex functions.
- **Consumers must pass the tag.** Build tags can't be set from `go.mod`. Documented in the README. No RepoDB-side build guard: consumers with ICU may use the ICU backend.
- **DECIMAL storage encoding is unchanged.** Rows, primary keys and index keys store decimals as text, until now shopspring `String()` (trailing zeros trimmed). The store is content-addressed, and index lookups compare encoded bytes, so the encoding must not change. Encode converts `*apd.Decimal` → shopspring → `String()`; decode parses with `apd.NewFromString` and converts to the column type. Verified cross-version: data written by the old binary (native-git and journal) is found by PK lookups, index lookups and uniqueness checks in the new one.
- **Embedded query results now return `*apd.Decimal`** for DECIMAL columns (previously shopspring `decimal.Decimal`), formatted at the column scale (`0.10`, not `0.1`), which matches MySQL. This follows GMS's value types, which `Session.Query` passes through unchanged. `TestDecimalRoundTrip` expectations updated. Parameter formatting accepts both types.
- **`CoversColumns` returns false.** It is only a cost-model hint (covering index → cheaper). RepoDB index lookups always fetch full rows, so no index is covering.
- **`context.Background()` for JSON encoding** instead of threading a context through ~40 encode call sites: the context is only used by lazily loaded JSON implementations (Dolt storage); RepoDB's values are in memory.
- **Go 1.26.2 floor** accepted by the user; supersedes rdb-d3c729's 1.25.

## Acceptance criteria

- go.mod requires GMS at the latest main pseudo-version; `go mod tidy` is clean.
- `make test` passes (includes the Windows cross build and vet with the tag).
- `go test -race -tags gms_pure_go ./common/repository ./engine ./integration` passes.
- govulncheck reports no vulnerabilities in any module.
- DECIMAL data written by the pre-upgrade binary remains readable and indexable.
- README documents Go 1.26 and the build tag.
