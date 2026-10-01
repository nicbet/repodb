# rdb-f4dec8 walkthrough: milestone leftovers retired

## Why

rdb-931f20 removed the milestone documents, but code still pointed at that era: two experiment programs, `make m0` / `m2-bench` / `m4.2-bench` / `m4.3-bench`, `BenchmarkM43*`, "prototype" comments, and user-visible errors saying "RepoDB M2 …". Agents read these as current workflows. The keep/remove list was agreed with the user before any change.

## What changed

**Deleted:** code that only its own tests exercised, or that newer tools supersede.
- `experiments/gitstorage`: the Git-integration prototype. It used only the standard library; `common/repository` and `integration` tests now cover the same Git behavior. Deleting it also drops ~5 s from `make test`.
- `experiments/m2bench`: superseded by `dbbench` and `BenchmarkSQL*`.
- `storage.Filesystem`: the runtime reads objects through Git (`cat-file --batch`). The `Store` interface and the in-memory store remain.
- `common/query`: the server parses through go-mysql-server.
- `Repository.CacheDir()`: nothing ever wrote there. `docs/cli.md` no longer describes a cache directory.
- `Writer.PendingHashes()`: no callers; found while rewording "prototype" comments.

**Renamed:** these still measure current behavior.
- `make bench-go` (was `m4.2-bench`): `BenchmarkSQL*` and the sync benchmarks.
- `make bench-journal` (was `m4.3-bench`): durable saves, journal replay growth, checkpoints.
- Benchmarks drop the `M43` prefix. `BenchmarkDurableSave` gets no `Journal` prefix because it compares both persistence modes. The others are `BenchmarkJournal*`.

**Reworded:**
- Error strings no longer name a milestone. The AUTO_INCREMENT/generated-column error now names what isn't supported instead of "unsupported M2 schema behavior".
- `docs/sql.md` quotes them verbatim and was updated. No test asserted the old text.
- Journal comments (`WorkingState`, `CommitTypedEdits`, `Checkpoint`) describe current behavior, not "the M4.3 prototype" or "the M4.4 path".

## Not changed

`docs/benchmarks/latest.md` (a published record) and xpo history. `common/robustio` stays: `integration` still uses it.

## Verification

- An acceptance `git grep` over the Go code, Makefile and docs is clean ("Apple M1 Max" in the README is a CPU).
- `make build`, `make test` and `make lint` pass, and each renamed benchmark runs one iteration.
