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
tree creation, commits, and ref updates with `core.fsync=committed,reference` and
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
| Primary-key hit inside an existing transaction, 50,000 rows | 12.8 ms | 158 us | 5.23 MB | 73.1 KB |
| Primary-key miss inside an existing transaction, 50,000 rows | 13.1 ms | 121 us | 5.23 MB | 63.8 KB |
| Range update of 1 of 10,000 rows + commit | 722 ms | 648 ms | 62.0 MB | 33.9 MB |
| Update 10 of 10,000 rows + commit | 606 ms | 518 ms | 62.2 MB | 35.1 MB |
| Update 100 of 10,000 rows + commit | 585 ms | 512 ms | 62.2 MB | 35.0 MB |
| Update 1,000 of 10,000 rows + commit | 671 ms | 638 ms | 62.1 MB | 35.3 MB |

Single-run exploration across 1,000, 10,000, and 50,000 rows showed the old
transaction start growing from 65 ms/4.0 MB to 313 ms/180 MB. Warm full scans
remain proportional to result size; at 50,000 rows they took about 14 ms and
allocated 14.7 MB after transaction setup. Ordered `LIMIT` queries still scan
and sort the whole table because RepoDB only advertises exact primary-key lookup,
not range ordering.

The warm lookup figures exclude the 13.5 ms transaction start and must not be
presented as standalone query latency. The original write matrix uses
`WHERE id >= 1 AND id <= ?`; its one-row case therefore includes a range scan.
It is useful for batch behavior but is not the best available point-update path.

## Exact-key incremental-write experiment

The focused decision experiment uses `UPDATE bench SET value = ? WHERE id = ?`
at the midpoint of deterministic 1,000, 10,000, and 50,000-row trees. Each result
below is the median of five independent one-operation processes. The baseline is
the unmodified commit named above; the after case includes the edit overlay,
canonical incremental mutation, and publication reuse. Durability settings are
identical.

| Rows | Baseline time | Incremental time | Baseline allocation | Incremental allocation | Rows decoded | New object writes |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 226 ms | 166 ms | 4.79 MB | 1.22 MB | 0 | 9 |
| 10,000 | 949 ms | 864 ms | 43.3 MB | 9.4 MB | 0 | 64 |
| 50,000 | 1.29 s | 968 ms | 210 MB | 25.4 MB | 0 | 59 |

The incremental path decodes no unrelated rows. Its latency grows sharply while
the changed content-defined chunk boundary propagates—from nine new objects in
this 1,000-row fixture to 64 at 10,000 rows—but it does not continue growing in
proportion to untouched rows: 10,000 and 50,000 rows write a similar number of
objects and differ by about 12% in median wall time. Allocation still grows with
the object inventory, but much more slowly than the bulk-rebuild baseline.

Median phase attribution (milliseconds per durable operation) was:

| Rows | Tree mutation | Reachability walk | Git object writes | Git tree + commit | Ref update | Verification |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 0.49 | 0.50 | 53.7 | 87.3 | 11.0 | 6.6 |
| 10,000 | 9.7 | 5.4 | 353 | 470 | 10.9 | 9.4 |
| 50,000 | 13.2 | 27.2 | 324 | 577 | 12.0 | 25.4 |

Git object and tree creation under the configured fsync policy now dominate;
lookup/edit and canonical mutation do not. This is the requested decision point:
the architecture has a viable proportionality path because a small write no
longer processes table rows and stops growing strongly after the affected chunk
sequence converges. It also exposes a real durability tradeoff: one Git commit
per SQL transaction currently has a roughly 0.15-second minimum under this
implementation and configuration in the smallest
fixture and approaches one second when a boundary change produces around 60 new
objects. This is not evidence of a general Git latency floor.

## Git hardening strategy experiment

A second controlled experiment distinguishes Git's capabilities from RepoDB's
current command strategy. Both variants use the explicit component set
`core.fsync=committed,reference`; only `core.fsyncMethod` changes between
`fsync` and `batch`. The benchmark copies one 50,000-row seed repository so both
variants start at the identical data commit and receive the identical sequence.
Every measured operation must advance the data ref and a query must confirm the
new value or deletion. Seed creation, repository copies, head checks, and result
checks are outside the Git command timers and counters.

Run five real mutations of each case with:

```sh
go test ./engine -run '^$' -bench '^BenchmarkGitDurabilityStrategies$' \
  -benchtime=5x -count=1
```

[Git Trace2](https://git-scm.com/docs/api-trace2) supplies full-flush and
writeout-only counters. Loose-object inventory
before and after each command supplies actual newly created object counts and
compressed on-disk bytes. `Blob inputs reused` counts submitted blob hashes that
were already present in Git; it is deliberately separate from new blobs.
Figures below are the median with the observed minimum--maximum in parentheses;
byte columns are five-operation means.

| Mutation | New blobs B | Blob KiB | Blob inputs reused | New trees T | Tree KiB | T/B |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Update first, short value | 55 (26--80) | 78.4 | 35 (0--97) | 69 (28--80) | 19.1 | 1.25 |
| Update quarter, 32 bytes | 9 (8--11) | 47.8 | 0 (0--2) | 12 (10--13) | 7.7 | 1.33 |
| Update middle, 4 KiB | 36 (8--58) | 59.1 | 0 (0--50) | 41 (12--81) | 15.1 | 1.14 |
| Update three-quarter, 32 bytes | 24 (14--65) | 54.1 | 64 (0--80) | 36 (19--70) | 15.7 | 1.50 |
| Update last, short value | 4 (4--4) | 26.1 | 0 (0--0) | 6 (6--6) | 6.3 | 1.50 |
| Insert short | 4 (4--4) | 26.1 | 0 (0--0) | 6 (6--6) | 6.2 | 1.50 |
| Insert 4 KiB | 4 (4--4) | 26.5 | 0 (0--0) | 6 (6--6) | 6.3 | 1.50 |
| Delete early | 16 (12--99) | 64.9 | 5 (0--85) | 22 (20--122) | 17.1 | 1.38 |
| Delete middle | 23 (12--77) | 56.1 | 1 (0--65) | 35 (23--85) | 15.4 | 1.52 |
| Delete late | 27 (9--64) | 45.1 | 40 (0--60) | 28 (11--63) | 13.5 | 1.04 |

Mutation location and successive value matter greatly: medians range from 4 to
55 new blobs and individual samples from 4 to 99. The hash-prefix object layout
adds another 6 to 69 median tree objects (6 to 122 individually). This confirms
that Prolly boundary propagation and Git directory updates amplify one another,
while showing that the former midpoint result near 60 blobs was not a universal
point-update cost.

Paired command medians in milliseconds and durability operations are:

| Mutation | fsync hash/tree/commit/ref | batch hash/tree/commit/ref | fsync full flushes | batch full flushes/writeouts |
| --- | ---: | ---: | ---: | ---: |
| Update first | 347/408/28/26 | 347/111/28/23 | 134 (56--162) | 58/69 (29--83 / 28--80) |
| Update quarter | 75/95/29/28 | 77/46/26/26 | 23 (20--26) | 12/12 (11--14 / 10--13) |
| Update middle | 230/263/31/30 | 232/83/33/29 | 79 (22--141) | 39/41 (11--61 / 12--81) |
| Update three-quarter | 180/259/36/34 | 179/87/36/32 | 62 (35--135) | 27/36 (17--68 / 19--70) |
| Update last | 55/71/36/36 | 53/47/34/34 | 12 (12--12) | 7/6 (7--7 / 6--6) |
| Insert short | 55/71/37/36 | 53/47/35/35 | 12 (12--12) | 7/6 (7--7 / 6--6) |
| Insert 4 KiB | 54/70/36/35 | 53/47/35/34 | 12 (12--12) | 7/6 (7--7 / 6--6) |
| Delete early | 133/167/38/38 | 137/75/39/36 | 41 (38--223) | 19/22 (15--102 / 20--122) |
| Delete middle | 173/246/40/41 | 176/87/43/39 | 55 (41--164) | 26/35 (15--80 / 23--85) |
| Delete late | 195/210/44/45 | 197/85/44/43 | 57 (22--129) | 30/28 (12--67 / 11--63) |

The counters exactly fit the observed model: `fsync` uses `B + T + 2` full
flushes, while `batch` uses `B + 3` full flushes and `T` writeout requests. The
two final full flushes are the separately invoked commit and explicitly hardened
reference; `write-tree` adds the third. Batching materially reduces tree time,
especially when prefix-directory amplification is large. Hash time is unchanged:
the current `hash-object --stdin-paths` invocation does not establish Git's
batch-capable object-database transaction, so every newly created blob still
receives a full flush. Separate commands also preserve separate flush boundaries.

This result makes the next experiments independent: use tree batching on
qualified filesystems, find a blob-writing path that genuinely batches hardening,
and reduce chunk/directory amplification where its distribution justifies it.
Trace2 establishes which operations Git requested; it does not prove power-loss
safety. [Git documents `batch`](https://git-scm.com/docs/git-config#Documentation/git-config.txt-corefsyncMethod)
as expected to be as safe as `fsync` on macOS with
HFS+ or APFS (and Windows with NTFS or ReFS), and only for loose objects. RepoDB
therefore retains the existing filesystem/device assumptions and recovery checks;
the production default remains `fsync` pending an explicit platform policy.

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
3. Transactions retain an edit/delete overlay and statements retain undo records
   only for keys they touch. Existing trees are mutated from sorted edits;
   affected leaf chunks and ancestors are rebuilt until canonical chunk boundaries
   realign. No-op updates do not dirty or publish.
4. Publication ignores newly generated blobs already verified in the base,
   carries immutable object IDs forward, updates the Git tree from its parent,
   and verifies paths and IDs after publication without rereading every blob.
   Unchanged tables retain exact schema, data, and reachable-object identities.
5. Synchronization checks the expected local candidate under the publication
   lock, then releases the lock before pushing that exact immutable commit. A
   blocking remote-hook test proves a concurrent SQL commit can publish locally;
   its descendant remains outgoing for the next bounded sync.

## Practical envelope and remaining work

RepoDB remains scoped to small tool databases: approximately 1,000 rows per
table, modest payloads, tens of writes per interactive transaction, and explicit
sync. Warm point reads are now insensitive to unrelated row count after the
transaction's small ref check. Exact-key mutations no longer decode or rebuild
the whole table, although canonical boundary propagation and durable Git object
and tree writes leave substantial latency. Cold open and validation still buffer
and decode the complete snapshot. The 50,000-row full-sync result still takes
about six seconds and allocates 371 MB cumulatively.

The evidence-ranked backlog is:

1. Reduce canonical boundary-propagation cost or explicitly evaluate a future
   key-stable chunking format; do not silently change existing roots.
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
