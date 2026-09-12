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

The engine and integration test suites passed after adding the benchmark. The
full test suite could not run its MySQL wire tests in the restricted benchmark
environment because binding a loopback socket was not permitted.
