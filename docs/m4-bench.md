# M4 merge benchmark

Measured 2026-09-12 on Linux/amd64 with an Intel Core i7-8550U CPU and Go
1.27.1.

M4 is functionally complete, but its merge path is not yet scale-efficient. At
50,000 rows, merging sparse independent changes takes approximately 0.76 seconds
and allocates 288 MB before the final Git commit or any network transport.

## Workloads

The benchmark creates one table with a `BIGINT` primary key, a short title, and
a boolean value. It measures two cases at 1,000, 10,000, and 50,000 rows:

- **Sparse changes:** local and remote each update a disjoint 1% of the rows.
- **Unchanged table:** base, local, and remote reference the same table version.

Fixture construction is excluded from the reported time. Each iteration includes
`Repository.BeginMerge` and `engine.MergeSnapshots`, including snapshot loading,
but excludes `Writer.CommitWithOutcome`, fetch, push, and remote latency. Results
below are medians of three runs with a 500 ms benchmark duration. The benchmark
harness is in `engine/merge_bench_test.go`.

## Results

| Rows | Sparse changes | Sparse allocations | Unchanged table | Unchanged allocations |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | ~72 ms | 5.36 MB | ~63 ms | 3.13 MB |
| 10,000 | ~288 ms | 58.5 MB | ~173 ms | 34.2 MB |
| 50,000 | ~763 ms | 288 MB | ~555 ms | 163 MB |

`BeginMerge` launches six Git processes per operation because it reloads the
local and remote snapshots. The allocation growth is approximately linear with
row count, but the bytes allocated per row are high: about 5.8 KB per row for the
50,000-row sparse-change workload.

Run the benchmark with:

```sh
go test ./engine -run '^$' -bench '^BenchmarkMergeSnapshots' \
  -benchmem -benchtime=500ms -count=3
```

## Opportunities

### 1. Reuse immutable table roots

`copyTable` currently reads, validates, and rebuilds an entire table even when
its schema and data roots are unchanged. Consequently, a completely unchanged
50,000-row table still takes approximately 555 ms and allocates 163 MB.

The merge should retain an existing table root directly when the selected table
is already present in the local writer base. When selecting a remote table, it
should import only reachable objects missing from the local snapshot rather than
materializing and rebuilding every row. This is the highest-value optimization,
especially for databases with many tables where only one table changed.

### 2. Stream the three-way row merge

`mergeRows` currently materializes all three trees into maps, creates a fourth
set containing all keys, sorts those keys, and copies the selected values. The
result is then validated row by row and passed to `prolly.Build`, which clones and
sorts it again.

Sorted Prolly-tree iterators would allow a three-way streaming merge. A builder
that accepts already sorted entries could emit chunks incrementally without the
extra clone and sort. This should substantially reduce peak memory while keeping
deterministic output.

### 3. Avoid loading snapshots twice

The synchronization path loads the base, local, and remote snapshots before
calling `BeginMerge`. `BeginMerge` then reloads local and remote to construct the
writer. Passing the validated snapshots into the writer, or caching snapshots by
immutable commit ID, would eliminate six Git subprocesses and two eager
object-graph copies per merge.

### 4. Fast-path an up-to-date sync

`integration.Sync` loads and validates the fetched remote snapshot before testing
whether the local and remote heads are equal. Since equal commit IDs identify the
same immutable snapshot, checking equality first would make routine no-op syncs
independent of database size apart from fetch/ref overhead.

### 5. Cache validation and batch Git object writes

Snapshot loading eagerly reads and hashes every listed object. Successful graph
validation can be cached by immutable commit ID so repeated sync attempts do not
decode the same tree and rows again.

During publication, each new RepoDB object is currently sent through a separate
`git hash-object` process. A batch object-writing interface would reduce process
startup overhead for large merges.

## Recommended sequence

1. Add the unchanged-root fast path and benchmark its effect.
2. Reuse already-loaded snapshots in `BeginMerge` and move the up-to-date check.
3. Add sorted Prolly iterators and a streaming merge/builder.
4. Measure complete local-remote synchronization, conflict-heavy workloads,
   validation caching, repository growth, and peak resident memory.

## M4.1 results

Measured 2026-09-12 on Darwin/arm64 with an Apple M1 Max and Go 1.27.1.
These timings are not directly comparable with the Linux/amd64 baseline above;
the allocation profiles and eliminated work identify the effect of the changes.

M4.1 now retains selected schema and data roots, reuses caller-supplied validated
snapshots, streams three ordered tree iterators into a canonical sorted builder,
caches successful SQL validation for 128 immutable repository/commit/version
keys, and writes new Git blobs in one batch. The iterator retains at most one
decoded node per input tree level and the builder at most one bounded chunk per
output level. Loaded snapshot object buffers, output objects, and accumulated
conflicts remain separately proportional to their inputs; the complete sync is
therefore not claimed to use constant memory.

The original microbenchmark command (500 ms, three samples) produced these
medians after M4.1:

| Rows | Sparse changes | Sparse allocations | Unchanged table | Unchanged allocations | Git processes/op |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 3.51 ms | 2.35 MB | 19.0 us | 135 KB | 0 |
| 10,000 | 34.5 ms | 24.2 MB | 177 us | 1.47 MB | 0 |
| 50,000 | 160 ms | 120 MB | 973 us | 7.35 MB | 0 |

The unchanged path no longer decodes or rebuilds rows. Its remaining allocation
growth is the selected table's cached reachable-object inventory copied into the
result set. Sparse merge allocation is still linear because all three validated
snapshots keep object bytes in memory, the output writer keeps new objects until
publication, and JSON decoding allocates row values. M6 should target those
buffers if larger supported workloads require it.

### Complete local-remote synchronization

`integration/sync_bench_test.go` uses a local bare remote and two clones. Fixture
creation, state reset, and repository-size inspection are outside the timer;
fetch, merge/conflict handling, publication, push, and the confirming fetch are
inside it. These are one-operation warm-cache measurements without real network
latency. Allocated bytes are cumulative. Peak RSS is the isolated benchmark
process high-water mark and includes fixture construction. Repository growth is
the local `.git` size delta and can include Git bookkeeping in addition to blobs.

| Workload | Time | Allocated | Peak RSS | Git processes | Object writes | Repository growth |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Up-to-date, 1,000 rows | 160 ms | 0.93 MB | 57.7 MB | 17 | 0 | 0 |
| Sparse, 1,000 rows | 748 ms | 7.87 MB | 58.1 MB | 42 | 14 | 28.7 KB |
| Sparse, 10,000 rows | 1.97 s | 78.0 MB | 67.6 MB | 42 | 129 | 306 KB |
| Sparse, 50,000 rows | 6.09 s | 371 MB | 129 MB | 42 | 637 | 1.52 MB |
| Remote-selected table, 1,000 rows | 469 ms | 5.86 MB | 58.7 MB | 42 | 1 | 4.93 KB |
| 50 tables, only one changed | 734 ms | 8.57 MB | 59.9 MB | 42 | 14 | 29.4 KB |
| Conflict plus resolution, 1,000 rows | 774 ms | 12.0 MB | 55.9 MB | 73 | 15 | 14.5 KB |
| One rejected-push retry, 1,000 rows | 1.06 s | 8.00 MB | 58.7 MB | 48 | 14 | 28.7 KB |

Run the complete path benchmarks with:

```sh
go test ./integration -run '^$' \
  -bench '^(BenchmarkSyncUpToDateWarm|BenchmarkSyncDivergent)$' \
  -benchmem -benchtime=1x -count=1
```

Run each sub-benchmark in a separate process when comparing peak RSS; `ru_maxrss`
is a process-wide high-water mark. Cold opens deliberately reconstruct and
validate snapshots from Git. Warm repeated validation is bounded by repository
identity, immutable commit ID, and validation version; eviction or process exit
falls back to full validation. Batch writes retain Git's `core.fsync=committed`
and `core.fsyncMethod=fsync` settings and do not delay publication durability.

The measurements retain the existing small-database support statement. In
particular, the full 50,000-row local synchronization still allocates 371 MB
cumulatively and takes about six seconds on this machine, so the microbenchmark
improvement alone does not justify broadening the workload claim.

The complete Go suite, including MySQL loopback tests, passed in an environment
where loopback binding was allowed. The repository, engine, and integration race
tests, both production builds, and `git diff --check` also passed.
