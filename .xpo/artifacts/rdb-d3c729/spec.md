# Lower go directive to 1.25

## What

Change `go.mod` from `go 1.27` to `go 1.25.0`, pin `golang.org/x/sys v0.47.0` and `golang.org/x/term v0.45.0`, and update the README's Go version requirement.

## Why

RepoDB is a library. The `go` directive is a minimum requirement imposed on every consumer, so it should be the lowest version the code and its dependencies actually need. `go 1.27` came from the initial scaffold recording the local toolchain; nothing depends on it.

## How

1. `go get golang.org/x/sys@v0.47.0 golang.org/x/term@v0.45.0`
2. `go mod edit -go=1.25.0`, run *after* `go get`, because `go get` raises the directive to 1.26.0 while x/sys v0.48.0 is still in the graph.
3. `go mod tidy`; confirm the directive stays `go 1.25.0`.
4. README Installation: "Go 1.27 or newer" → "Go 1.25 or newer".

## Decisions

- **Floor is 1.25, not lower.** `gofrs/flock v0.13.1` (Windows lock support, rdb-52da05) requires 1.25. Going lower would mean downgrading flock; out of scope.
- **No security regression.** x/sys v0.48.0 and x/term v0.46.0 contain no security fixes (feature additions, `unsafe.Add`/range-int cleanups, a `ReadLine` partial-data fix RepoDB does not use). govulncheck is clean for the lower versions. Because of minimal version selection these are minimums, and consumers requiring newer versions still get them.
- **Historical Go 1.27 mentions stay.** Benchmark docs and xpo artifacts record what was measured, not requirements.
- **Stale POSIX-locking wording** is tracked separately in rdb-d86f52.

## Acceptance criteria

- `go.mod` declares `go 1.25.0` with x/sys v0.47.0 and x/term v0.45.0; `go mod tidy` is a no-op.
- `make test` passes (includes the GOOS=windows build + vet).
- `go vet ./...` passes (`stdversion` flags no stdlib API newer than 1.25).
- README states Go 1.25 or newer.
