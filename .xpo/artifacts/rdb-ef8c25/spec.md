# Spec: Windows cross-compile check in `make test`

## What
Add a `cross-windows` Makefile target that runs `GOOS=windows GOARCH=amd64 go build ./...` and `go vet ./...`, and make `test` depend on it.

## Why
Development happens on macOS, so a new Unix-only call would go unnoticed until a Windows user hit it. `go vet` type-checks `_test.go` files too, so test code stays portable.

## How
```make
cross-windows:
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=windows GOARCH=amd64 go vet ./...

test: cross-windows
	go test ./...
```
Add `cross-windows` to `.PHONY`.

## Decisions
- **Prerequisite, not a trailing recipe line:** the cross-compile (a few seconds) runs before the full test suite (~1 min), so portability breaks fail fast. Make stops on the first failing recipe line, so either Windows step failing fails `make test`.
- **Target name `cross-windows`:** leaves room for other cross targets later without renaming. It can also be run on its own.
- **amd64 only**, as the issue specifies. arm64 uses the same OS-level APIs, so it adds little.

## Acceptance criteria
- `make test` runs the Windows build + vet before `go test`.
- `make test` exits non-zero if either Windows step fails (checked by temporarily adding a Unix-only call).
- `make test` passes on the current tree.

## Out of scope
Running tests on a real Windows host.
