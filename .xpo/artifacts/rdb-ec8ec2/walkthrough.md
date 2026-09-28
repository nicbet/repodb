# Walkthrough: one canonical latest benchmark results file

## Problem

"How fast is RepoDB?" had several answers: `docs/benchmark.md` § Reference comparison, `performance.md` (M4.2), `m4-bench.md` (M4/M4.1), `m4.3-bench.md`, four scorecard JSONs, a README table, and numbers scattered in `sql-m2.md`, `guide.md` and `working-state.md`. The "reference comparison" was itself stitched together from four revisions (one with a dirty tree), and all of it predated the go-mysql-server upgrade and the three correctness fixes.

## The layout

```
docs/benchmark.md               methodology only (how to run, workload contract,
                                reading results, baselines, coverage limits,
                                publishing process)
docs/benchmarks/latest.md       the one place that states current numbers
docs/benchmarks/latest/         raw JSON behind latest.md
docs/benchmarks/history/        dated, frozen reports
```

History entries: `2026-09-11-m2-sql-baseline`, `2026-09-12-m4-merge-sync`, `2026-09-12-m4.2-sql-performance`, `2026-09-13-m4.3-working-state`, and `2026-09-13-m4.4-scorecard` (plus its JSON directory). All were moved with `git mv`, so `git log --follow` still works.

## History is frozen; caveats go in the banner

Every history file starts with a `> **Historical record.**` banner: what was measured, when, at which revision(s), on which machine, "describes the repository at that time and is not maintained", and a link to latest.md. The report bodies are untouched apart from repaired relative links. Anything we have since learned goes in the banner:
- **M4.3:** its checkpoint timings came from a benchmark path that dropped pending row edits (fixed in rdb-3745e6), so they understate checkpoint cost.
- **M4.4:** its text says all correctness checks passed, but its own Dolt JSON records `contended_increment` lost-update failures (120 acknowledged, 40 stored) and serialization failures. This was found while preparing rdb-6ae82d; Dolt 2.3.3–2.3.5 still lose concurrent autocommit increments.

## The publishing process (benchmark.md § Publishing results)

1. Measure every mode at one clean commit. Each JSON's `revision` must match, and `working_tree_status` must be empty.
2. `git mv` the old latest.md and its JSON into `history/<date>-<short-rev>-scorecard.*`, and add the banner.
3. Write the new latest.md with revision, date, machine, OS, filesystem, Go, Git, server versions, commands and JSON links.
4. Update the README table from it.

Diagnostic or milestone studies go straight to history; they never become latest.

## Interim latest.md

Until rdb-6ae82d measures a clean run, latest.md says plainly that none exists yet, lists what has changed since 2026-09-13, and links the old report. It deliberately does not copy the old tables: that would duplicate history and present mixed-revision, pre-fix numbers as "latest". The README table keeps the old numbers under a "measured 2026-09-13 across mixed revisions, fresh run pending" caption.

## Other cleanup

- README: links point to latest.md for results and benchmark.md for methodology. The "Benchmarks" entry replaces "Scorecard", and the `make bench` comment is fixed: it defaults to native-git, not journal.
- `sql-m2.md`: its dated M2 measurement paragraph moved to history.
- `guide.md`: "~5 ms per transaction" is now the mechanism (one fsync) plus a link.
- `working-state.md`: keeps its design rationale number, now citing the M4.3 report.
- `plan.md`: a historical plan, so only its links changed.

A link-and-anchor check over README and all `docs/**/*.md` passes.
