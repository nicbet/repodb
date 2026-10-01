# Testing

This page describes RepoDB's test surface: what each suite protects, how to run it, and what is not covered. See [architecture.md](architecture.md) for the components named here.

## Running

The Makefile sets `GOFLAGS=-tags=gms_pure_go`. When calling `go` directly, pass the tag yourself.

| Command | Runs |
| --- | --- |
| `make test` | `GOOS=windows go build ./...` and `go vet ./...`, then `go test ./...` |
| `go test -race -tags gms_pure_go ./common/repository ./engine ./integration` | the concurrency-heavy packages under the race detector (not part of `make test`) |
| `make lint` | `gofmt` check; `go vet` and staticcheck for the host and for Windows |
| `go test -tags gms_pure_go -cover ./...` | statement coverage per package. Add `-coverpkg=./...` to credit code exercised from other packages: `common/repository` and `common/git` are mostly driven by the `engine` and `integration` tests. |
| `go test -tags gms_pure_go ./engine -run '^$' -fuzz '^FuzzDecodeRow$' -fuzztime 60s` | one fuzz target in fuzzing mode (see [Fuzz targets](#fuzz-targets)) |
| `go test -tags gms_pure_go ./engine -run '^$' -bench <name> -benchmem` | Go benchmarks (see [Benchmarks](#benchmarks)) |
| `make bench`, `make bench-external` | the database scorecard ([benchmark.md](benchmark.md)) |

**Requirements.** Tests need `git` on `PATH`: most packages create real repositories, bare remotes, clones and linked worktrees. The tests use `git init -b`, so Git 2.28 or newer is needed. A few tests install `/bin/sh` hook scripts. No test needs MySQL, Dolt or Docker. The server tests listen on a random loopback port.

The full suite takes about a minute and a half. `engine` is the slowest package, at about a minute.

## What the suites protect

### Storage core

- **`common/prolly`.** The Prolly trees. Tests cover:
  - deterministic, readable builds;
  - rejection of duplicate and unordered keys;
  - the streaming builder matching a bulk build;
  - history independence: `Apply` of any edit sequence gives the same root as a fresh build, from 0 to 10k entries and for distant edits;
  - seeking iterators at exact keys, between keys, past the end, and on empty trees.
- **`common/storage`.** The filesystem store: round trips, 16 concurrent writers of one object, a writer that loses a rename race, and rename failure. The Windows-style races are simulated through a replaceable `rename` function.
- **`common/git`.** Durability auditing: which Git commands flush objects and refs, under `fsync` and `batch` methods. Also that pushing the data ref skips the host repository's `pre-push` hook.
- **`common/robustio`.** The retry loop: success after transient failures, stopping on permanent errors, and the timeout.

### Repository and publication (`common/repository`)

- **Snapshot round trip.** A snapshot survives cache deletion, `git gc --prune=now`, reopening, a push to a bare remote, and a clone and fetch, while the source worktree's `HEAD` and status stay untouched.
- **Optimistic publication.** A stale writer is rejected with `ErrConflict`. Of two concurrent writers, exactly one succeeds. A cancelled lock wait is a definite rejection.
- **Commit outcomes.** Faults injected before, during and after the ref update give `rejected`, `unknown` and `committed`, and `RecoverCommit` resolves each.
- **Multiple processes:**
  - two writer processes in linked worktrees give one success and one conflict;
  - a writer killed inside publication leaves the old snapshot intact and releases the lock;
  - linked worktrees share one repository identity.
- **Formats and corruption.** Missing data gives `ErrNotInitialized`. A future format and the previous format are both refused. A manifest listing a missing object gives `ErrCorrupt`.
- **SHA-256 Git repositories.** A round trip; the test is skipped if the installed Git lacks SHA-256 support.

### SQL engine (`engine`)

- **SQL behavior** (`engine_test.go`, about 150 tests; default journal mode, with native-git variants):
  - commit, rollback, parameters and escaping;
  - stale-transaction rejection;
  - failed statements that don't leak partial changes;
  - DDL implicit commit;
  - `ALTER TABLE` (add, drop, modify and rename columns; composite and changed primary keys; persistence across reopen);
  - `DEFAULT`, `CHECK` and `NOT NULL`;
  - temporal, JSON, DECIMAL and ENUM types;
  - collations (case-insensitive comparison, ordering and key uniqueness);
  - `UNIQUE` constraints and secondary indexes, including how they are maintained on update and delete.
- **Unsupported DDL** (`ddl_honored_test.go`). Views, JSON key columns, index prefix lengths, `FULLTEXT` indexes and primary-key changes fail with clear errors; `TIME(p)` precision persists across reopen in both modes. `keycodec_test.go` checks that `keyable` agrees with the key codec for every stored type.
- **Journal** (`working_test.go`):
  - rows survive a restart without advancing Git;
  - one checkpoint publishes one snapshot;
  - stale writers are rejected;
  - an incomplete tail is ignored and checksum corruption is detected;
  - recovery after a fault following the flush;
  - recovery of a checkpoint published before its bookkeeping;
  - separate engines observing each other;
  - incremental replay from a cached offset, and detection of a replaced journal file;
  - bounded snapshot loading during replay;
  - the native-git open guard;
  - `Diff` listing row-only inserts, updates and deletes, new tables, and pending edits after a reopen.
- **Cross-engine visibility** (`cross_engine_test.go`). Readers see other engines' writes in every mode, including external `git update-ref` and `pack-refs` changes, without a restart.
- **Range queries** (`range_test.go`). Indexed range and `ORDER BY … LIMIT` results are compared with a forced full scan. The comparison runs over native-git, journal with pending edits, and open transactions, and over signed-integer, decimal, collated-string and datetime keys, using seeded random data. Tests also cover integer extremes, and that plans use `IndexedTableAccess` without a sort.
- **Journal index overlay** (`journal_index_test.go`). Inserts, updates and deletes are visible through persisted indexes, with a random-operation model test (150 steps, checkpoints and reopens, one or two engines).
- **Merge** (`merge_performance_test.go`). A merge spawns no Git processes, unchanged table roots are reused, secondary indexes are rebuilt, and point lookups do bounded work.
- **Codecs.** `rowcodec_test.go` covers every type at its extremes, canonical bytes, compact integers, and rejection of truncated or trailing bytes. `keycodec_test.go` checks that encoded key order equals SQL order for each key type (random and edge values), composite key order, and rejection of non-key values.
- **Read-only transactions** (`readonly_test.go`). Writes are rejected with the read-only error, reads still work, and the session can write again after `COMMIT`.
- **EXPLAIN rewriting** (`explain_test.go`).

### Sync and merge (`integration`)

- **End to end.** One scenario with a bare remote and two clones covers:
  - `enable` (initialize-empty, adopt-remote, idempotent, no push refspec, no hooks);
  - push and fast-forward, with a pinned transaction isolated from the fast-forward;
  - a disjoint merge producing a two-parent commit;
  - a row conflict that leaves the ref untouched and is saved, then resolved;
  - an update/delete conflict;
  - a clean source worktree at the end.
- **Other cases:**
  - sync refuses a dirty journal;
  - sync does not hold the publication lock while pushing (a remote hook stalls the push while a SQL commit completes);
  - incompatible schema changes are reported as schema conflicts;
  - `enable` rejects an invalid fetched SQL graph;
  - the default remote comes from config.

### Server and CLI

- **`server`.** A MySQL-protocol round trip with the Go client: exec, query, parameters, `EXPLAIN` as a plan, `DESCRIBE`, and restart. Also commit-outcome errors carried over the wire and recovered with `RecoverCommit`. A write in a read-only transaction returns MySQL error 1792, and the connection keeps working.
- **`cmd/repodb`.** `init`, then `commit`: journal rows are checkpointed and the journal is clean afterwards.
- **`experiments/dbbench`.** The scorecard harness's bookkeeping: rejected-attempt counting, partial reports, percentiles, and tolerance of files removed by Git's auto-gc.

## Test infrastructure

- **Fault injection.**
  - `Repository.SetPublicationFaultInjector` fires at `BeforeRefPublication`, `DuringRefPublication` and `AfterRefPublication`.
  - `WorkingState.SetFaultInjector` fires at `BeforeJournalAppend`, `BeforeJournalFlush`, `AfterJournalFlush` and `AfterCheckpointRef`.
- **Subprocess tests.** They re-execute the test binary as helper writers, coordinate through file barriers, and kill a helper mid-publication.
- **Counters.** The engine's performance counters, journal metrics, Git process and object-write counts, and publication metrics let tests assert how much work an operation did, not only its result.
- **Peak memory.** Benchmarks report peak RSS on Unix.

## Fuzz targets

All fuzz targets are in `engine` (`fuzz_test.go`, `rowcodec_test.go`). A plain `go test` runs only their seed inputs; there is no committed corpus and no scheduled fuzzing run. Failing inputs found while fuzzing are written to `engine/testdata/fuzz/<Target>/`, and should be committed with the fix.

| Target | Property |
| --- | --- |
| `FuzzSqlLiteral` | a literal produced for any string survives parameter binding unchanged |
| `FuzzBindNoPanic` | binding any statement and argument never panics |
| `FuzzRowRoundTripInt64`, `…Uint64`, `…Float64`, `…String`, `…Blob` | encoding then decoding a one-column row returns the value (Float64 skips NaN and ±Inf) |
| `FuzzDecodeRow` | decoding arbitrary bytes fails cleanly. Anything that decodes survives another encode and decode unchanged. |
| `FuzzSchemaRoundTrip` | a schema's column names, nullability, primary-key ordinals and checks survive encoding (column type fixed to BIGINT) |
| `FuzzEncodeKey`, `FuzzEncodeKeyString` | key encoding is deterministic |
| `FuzzEncodeKeyDistinct` | distinct integers produce distinct keys |

## Benchmarks

| Area | Benchmarks |
| --- | --- |
| SQL latency | `BenchmarkSQLTransactionBeginRollback`, `BenchmarkSQLWarmReads`, `BenchmarkSQLWriteBatches`, `BenchmarkSQLExactKeyUpdate`, `BenchmarkSQLAutocommitPointRead`, `BenchmarkSQLAutocommitScans` |
| Journal | `BenchmarkM43DurableSave`, `…JournalCheckpoint`, `…JournalReplayGrowth`, `…JournalFirstSaveAfterCheckpoint`, `…TypedEditJournal`, `…TypedEditCheckpoint`, `BenchmarkJournalIndexedInsertWithPending` |
| Git durability | `BenchmarkGitDurabilityStrategies` (fsync vs batch; uses `cp -R`, Unix only) |
| Merge and sync | `BenchmarkMergeSnapshotsSparseChanges`, `BenchmarkMergeSnapshotsUnchangedTable`, `BenchmarkSyncUpToDateWarm`, `BenchmarkSyncDivergent` |
| Codec | `BenchmarkRowCodec` |

The end-to-end comparison with MySQL and Dolt is `experiments/dbbench` ([benchmark.md](benchmark.md)). Current results are in [benchmarks/latest.md](benchmarks/latest.md).

## Gaps

These are known holes in the test surface:

- **Platforms.** The suite runs on macOS only. Linux shares the Unix code paths but is not run. On Windows only build, `vet` and staticcheck run. The Windows-only code (`LockFileEx` locking, retried renames) is never executed, and several tests assume `/bin/sh` and Unix file modes.
- **No CI.** Nothing runs the suite automatically.
- **Race detector.** It is not part of `make test`.
- **CLI.** Only `init` and `commit` are tested; argument handling, output and exit codes are not. `cmd/repodb-server` has no tests.
- **Client.** `client` has no tests of its own; it is exercised only through the server tests.
- **Journal faults.** `BeforeJournalAppend` and `BeforeJournalFlush` are never injected, and no test kills a process during a journal append or checkpoint.
- **Sync faults.** No faults are injected during sync, fetch or push beyond a stalled push. The sync/journal race (rdb-e0c717) has no test.
- **SQL compatibility.** Nothing compares RepoDB's SQL results with MySQL's (rdb-686c51).
- **Git object formats.** Only one test uses a SHA-256 repository.
- **Load.** There are no stress or soak tests beyond two-writer races and the 150-step index model.
