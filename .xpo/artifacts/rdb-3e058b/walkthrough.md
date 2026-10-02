## What changed
One sentence in README.md's Performance section. The old text said "Neither MySQL nor Dolt provides Git-native version history or cross-clone synchronization." That is false for Dolt. Dolt has Git-style commits, branches and merges, syncs across clones with push and pull, and since v1.81.10 (February 2026) can use plain Git remotes such as GitHub.

The sentence now explains why the sync results have no baseline: the benchmark harness runs sync and merge workloads only against RepoDB (see docs/benchmark.md, "External baselines").

## Why this wording
The sentence was there to explain a gap in the benchmark tables, not to compare features. Describing what the harness measures can be checked against the code. Statements about what other products cannot do go stale as those products change, and this one already had.

## For future readers
Before writing a comparative claim about MySQL, Dolt or another system, check it against that project's current documentation and cite the source in the issue. When the claim only explains benchmark coverage, describe the harness rather than the other product.