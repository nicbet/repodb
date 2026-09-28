# M2 SQL correctness baseline (2026-09-11)

> **Historical record.** M2 `make m2-bench` measurement, taken 2026-09-11 on a macOS
> development machine with Git 2.55 and recorded in `a966a9e`; moved here from
> [sql-m2.md](../../sql-m2.md). It describes the repository at that time and is not
> maintained. Current results are in [latest.md](../latest.md).

On a 2026-09-11 macOS development run with Git 2.55, 100 rows and ten autocommit
updates measured approximately 68 ms open, 60 ms begin, 156 ms bulk commit,
1.63 seconds for all updates, 4.6 MB heap, four live objects, and 199 Git
subprocesses. Snapshot object reads are batched and unchanged Git blob IDs are
reused, but process startup remains a visible cost. M3 or a later performance
pass should replace more per-operation CLI plumbing before increasing the
supported workload.
