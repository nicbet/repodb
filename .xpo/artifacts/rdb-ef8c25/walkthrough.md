# Walkthrough: Windows cross-compile check in `make test`

## What was built and why

rdb-52da05 made RepoDB compile for Windows. Nothing kept it that way: development happens on macOS, where a new `x/sys/unix` call or Unix-only syscall constant compiles fine. The breakage would only show up when a Windows user tried to build. This change makes every `make test` run check Windows compilation.

## The change

One file, `Makefile`:

```make
test: cross-windows
	go test ./...

cross-windows:
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=windows GOARCH=amd64 go vet ./...
```

`cross-windows` is also in `.PHONY` so a stray file named `cross-windows` can't stop it from running.

## How it works and why it's shaped this way

- **`go build` with `GOOS=windows`** type-checks and compiles every non-test package for Windows. No Windows toolchain is needed, since pure-Go cross-compilation is built in. The repo has no cgo, so this is complete coverage.
- **`go vet`** is included because `go build ./...` ignores `_test.go` files, while vet type-checks them. Without it, a Unix-only call in test code (like the old `unix.Flock` in `repository_test.go`) would slip through.
- **Prerequisite rather than an extra recipe line:** Make runs `cross-windows` before `test`'s own recipe. The Windows check takes seconds and the suite takes about a minute, so a portability break fails fast. Make stops on the first recipe line that exits non-zero, so a failure in either Windows step fails `make test`.
- **amd64 only:** portability problems in this codebase are OS-level (syscalls, build tags), not architecture-level. rdb-52da05 checked arm64 once by hand, and amd64 is enough for the ongoing check.
- **Separate named target:** it can be run on its own (`make cross-windows`), and there's room to add other platform targets later.

## Verification

- `make test` passes on the current tree.
- A temporary Unix-only reference in a non-test file failed at the `go build` step (`make` exit 2).
- The same reference in a `_test.go` file got past the build step and failed at the `go vet` step, before `go test` ran. This confirms vet is doing its job.

## Limits

This catches compile-time portability only. Runtime differences on Windows (file rename semantics, paths, git invocation; see rdb-207d5c, rdb-6e40a9) need tests on a real Windows host. That was deliberately deferred.
