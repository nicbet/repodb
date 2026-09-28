# Walkthrough: lower go directive to 1.25

## What changed

- `go.mod`: `go 1.27` → `go 1.25.0`; `golang.org/x/sys` v0.48.0 → v0.47.0; `golang.org/x/term` v0.46.0 → v0.45.0. `go.sum` updated accordingly.
- `README.md`: build requirement now reads "Go 1.25 or newer".

No Go source changed.

## Why

For a library, the `go` directive in `go.mod` is a floor imposed on every consumer: a project on Go 1.25 could not depend on RepoDB while it declared 1.27. The 1.27 value came from the initial scaffold commit recording whatever toolchain was installed, not from any real requirement.

## How the floor was determined

The directive must be at least the highest `go` line among the dependencies:

| Module | Declares |
| --- | --- |
| gofrs/flock v0.13.1 | 1.25.0 |
| x/sys v0.47.0 / x/term v0.45.0 | 1.25.0 |
| x/sys v0.48.0 / x/term v0.46.0 | 1.26.0 |
| go-sql-driver/mysql, edwards25519 | 1.24.0 |
| go-mysql-server v0.20.0 | 1.23.3 |

x/sys and x/term were the only things pushing past 1.25, and they can step back one release. flock sets the real floor at 1.25. Going lower would mean downgrading flock, which carries the Windows lock support from rdb-52da05.

RepoDB's own code has no newer requirement: `go vet` includes the `stdversion` analyzer, which flags stdlib APIs newer than the declared `go` version, and it passes on both the native platform and GOOS=windows.

## Is downgrading x/sys / x/term safe?

Yes. The v0.47→v0.48 x/sys diff adds constants and helpers (Windows `SO_SNDTIMEO`, Linux `IoctlPidfdInfo`, RISC-V/POWER10 CPU detection), regenerates Linux tables, and modernizes code (`unsafe.Add`, `for range n`), which is why it needs 1.26. The x/term v0.45→v0.46 change makes `Terminal.ReadLine` keep data returned alongside a read error. Neither contains security fixes, and govulncheck is clean for the lower versions. RepoDB only uses `term.IsTerminal`, `unix.Getrusage`/`Rusage` and `windows.ERROR_*` constants, and none of them changed.

Because of minimal version selection, these `require` lines are minimums. A consumer (or a future security fix) that needs x/sys v0.48.0+ still gets it. We can raise them again when there's an actual reason.

## Gotcha for future edits

`go get` raises the `go` directive automatically when anything in the graph requires more. Running `go get x/sys@v0.47.0 x/term@v0.45.0` set it to 1.26.0 because v0.48.0 was still in the graph during resolution. Always run `go mod edit -go=…` *after* `go get`, then confirm `go mod tidy` leaves it alone.

## Not changed

- Go 1.27 mentions in benchmark docs and xpo artifacts record measurement environments, not requirements.
- The stale "requires POSIX file locking" wording in README and `docs/storage-format.md` is tracked in rdb-d86f52.
