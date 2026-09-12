# RepoDB database performance

Status: M4.2 baseline and first bounded optimization pass, measured 2026-09-12.

## Reproducing the measurements

The SQL runner is `engine/sql_bench_test.go`; complete local-remote workloads are
in `integration/sync_bench_test.go`. Run both with:

```sh
make m4.2-bench
```

For controlled peak RSS, run one benchmark selector per fresh process. Go
benchmark fixtures are constructed before the timer starts. The fixtures use a
deterministic ascending integer primary key, a 32-byte text payload, and the
row counts printed in benchmark names. Write batches update keys 1 through N in
one explicit transaction and use a new value per measured iteration. Sync uses
a local bare remote and two clones, as described in [m4-bench.md](m4-bench.md).

The first controlled run used commit
`81d947289fe4d83cc77d66d2b61710b8ef0895a5` plus the working-tree changes under
measurement, Go 1.27.1, Git 2.55.0, macOS 15.7.7, and an Apple M1 Max. The Git
repository was on the local APFS workspace volume. RepoDB invokes object writes,
tree creation, commits, and ref updates with `core.fsync=committed` and
`core.fsyncMethod=fsync`; no repository override weakened those settings.
Filesystem caches were warm. These are local-development characterization
results, not cross-machine latency targets.

Use `-count=5` for independent benchmark samples and retain the complete Go
output as raw results. Profile separately from headline runs, for example:

```sh
go test ./engine -run '^$' -bench '^BenchmarkSQLWriteBatches' \
  -benchtime=5x -count=1 -cpuprofile cpu.out -memprofile memory.out
go tool pprof -http=:0 cpu.out
```

## Controlled before and after

The pre-change tree was recreated from the commit above in a separate temporary
directory and given the identical benchmark file. The figures below use 5
iterations for transaction start, 10 for point reads, and 3 for writes. Times
are means reported by Go for those iterations; allocations are cumulative per
operation. More independent samples are required before treating tail latency
as stable, so p95/p99 are deliberately not claimed here.

| Operation | Before | After | Before allocation | After allocation |
| --- | ---: | ---: | ---: | ---: |
| Begin + rollback, 50,000 rows | 307 ms | 13.5 ms | 179 MB | 58.6 KB |
| Warm primary-key hit, 50,000 rows | 12.8 ms | 158 us | 5.23 MB | 73.1 KB |
| Warm primary-key miss, 50,000 rows | 13.1 ms | 121 us | 5.23 MB | 63.8 KB |
| Update 1 of 10,000 rows + commit | 722 ms | 648 ms | 62.0 MB | 33.9 MB |
| Update 10 of 10,000 rows + commit | 606 ms | 518 ms | 62.2 MB | 35.1 MB |
| Update 100 of 10,000 rows + commit | 585 ms | 512 ms | 62.2 MB | 35.0 MB |
| Update 1,000 of 10,000 rows + commit | 671 ms | 638 ms | 62.1 MB | 35.3 MB |

Single-run exploration across 1,000, 10,000, and 50,000 rows showed the old
transaction start growing from 65 ms/4.0 MB to 313 ms/180 MB. Warm full scans
remain proportional to result size; at 50,000 rows they took about 14 ms and
allocated 14.7 MB after transaction setup. Ordered `LIMIT` queries still scan
and sort the whole table because RepoDB only advertises exact primary-key lookup,
not range ordering.

Complete sync measurements, including equal heads, sparse divergence at all
three row counts, selected and many unchanged tables, conflict resolution, a
rejected-push retry, repository growth, process counts, object writes, and peak
RSS, remain in [the M4.1 report](m4-bench.md). They use the same benchmark target
and form the current sync portion of the M4.2 runner.

## Attribution and implemented changes

The baseline confirmed three avoidable costs and the implementation now exposes
process-wide structural counters through `engine.ReadPerformanceCounters`:
decoded rows, scan rows, point keys visited, tables rebuilt/reused, and undo rows
captured. Regression tests assert these counts rather than imposing fragile
wall-clock limits.

1. A warm transaction now checks the live ref and reuses the engine's validated
   immutable snapshot. Table metadata is loaded lazily and row maps are decoded
   only for scans or edits. This removed database-size work from transaction
   start while preserving fresh-head inspection and pinned readers.
2. The SQL adapter advertises the supported primary key as a precise point-only
   index. Equality hits and misses visit only encoded candidate keys and read the
   corresponding Prolly path. Range pushdown is intentionally absent because the
   current key encoding is not generally SQL-order-preserving.
3. Statements retain undo records only for keys they touch. No-op updates do not
   dirty or publish. Commits rebuild dirty tables only and retain exact schema,
   data, and reachable-object identities for unchanged tables.
4. Synchronization checks the expected local candidate under the publication
   lock, then releases the lock before pushing that exact immutable commit. A
   blocking remote-hook test proves a concurrent SQL commit can publish locally;
   its descendant remains outgoing for the next bounded sync.

## Practical envelope and remaining work

RepoDB remains scoped to small tool databases: approximately 1,000 rows per
table, modest payloads, tens of writes per interactive transaction, and explicit
sync. Warm point reads are now insensitive to unrelated row count after the
transaction's small ref check, but a mutation still decodes and rebuilds its
whole table. At 10,000 rows that path remains roughly 0.5–0.65 seconds and 34–35
MB per commit on this machine. Cold open and validation still buffer and decode
the complete snapshot. The 50,000-row full-sync result still takes about six
seconds and allocates 371 MB cumulatively.

The evidence-ranked backlog is:

1. Add a transaction edit/delete overlay and incremental Prolly mutation so
   writes scale with touched keys rather than the dirty table.
2. Stream or lazily read Git objects and bound validation caches by bytes; this
   targets cold open, external-head adoption, and full-sync memory.
3. Add lazy full-scan iteration and safe limit/range pushdown only after defining
   SQL-order-preserving key behavior for the supported type subset.
4. Reduce the remaining fixed Git/ref/transport processes and measure controlled
   latency injection, packed histories, and long histories before changing the
   backend or durability model.

Larger payloads, multiple populated large tables, persistent MySQL latency,
CLI process startup, 1/2/4-writer distributions, packed-versus-loose histories,
transmitted bytes, Git-child RSS, and process-cold versus filesystem-cache-cold
runs remain required before M4.2 can be marked complete. The runner is structured
to extend those dimensions without changing the measured operations.
