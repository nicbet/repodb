# One canonical latest benchmark results file

## What

Make `docs/benchmarks/latest.md` the only document that states RepoDB's *current* performance, move every past report into `docs/benchmarks/history/` as a dated artifact, and document how a new run replaces latest.

## Why

Current numbers live in four places:
- `docs/benchmark.md` § "Reference comparison";
- `docs/performance.md` (M4.2);
- `docs/m4-bench.md` (M4/M4.1 merge and sync);
- `docs/m4.3-bench.md` (working state);

plus `docs/scorecard-*.json`, and the README copies a table. It is unclear which numbers are current. The "reference comparison" itself mixes four revisions, one with a dirty tree, all before the go-mysql-server upgrade and today's fixes.

## Layout

```
docs/benchmark.md                         methodology only: how to run, workload contract,
                                          reading results, external baselines, coverage
                                          limits, and the publishing process (new)
docs/benchmarks/latest.md                 the canonical current results
docs/benchmarks/latest/*.json             raw reports behind latest.md (from rdb-6ae82d)
docs/benchmarks/history/
  2026-09-11-m2-sql-baseline.md           ← the dated measurement paragraph from sql-m2.md
  2026-09-12-m4-merge-sync.md             ← docs/m4-bench.md
  2026-09-12-m4.2-sql-performance.md      ← docs/performance.md
  2026-09-13-m4.3-working-state.md        ← docs/m4.3-bench.md
  2026-09-13-m4.4-scorecard.md            ← benchmark.md "Reference comparison" +
                                            "Initial harness verification" sections
  2026-09-13-m4.4-scorecard/*.json        ← docs/scorecard-*.json
```

Files are moved with `git mv` so their history follows.

## Rules

- **History is frozen.** Each history file gets a banner as its first lines: a historical record, measured on <date> at <revision(s)>, describing the repository at that time, not maintained, and pointing to latest.md for current results. Bodies stay verbatim; only relative links are repaired. Known caveats go in the banner, not the body: the M4.3 checkpoint timings came from the path fixed in rdb-3745e6.
- **latest.md** always states: the revision (exactly one, clean tree), the date, the machine/OS/filesystem, Go, Git, the external server versions, the exact commands, and links to its raw JSON. Its tables follow the scorecard's reading order: single-client SQL latency, concurrency, sync.
- **Interim state (revised).** Until rdb-6ae82d runs, latest.md states that no clean single-commit run of the current code exists yet. It lists what has changed since, and links the 2026-09-13 report. It deliberately does not copy those tables. Copying would duplicate the history file and present mixed-revision, pre-fix numbers as "latest". The README table keeps the 2026-09-13 numbers under an explicit "measured 2026-09-13 across mixed revisions, fresh run pending" caption.
- **Publishing process** (in benchmark.md):
  1. Measure every mode at one clean commit.
  2. `git mv` the current latest.md and its JSON into `history/<date>-<short-rev>-scorecard.*`, and add the banner.
  3. Write the new latest.md.
  4. Update the README table from it.

  Milestone and diagnostic reports (Go microbenchmarks, experiments) go straight into history with a banner.
- **README.** The performance table cites latest.md and names its measurement date. Links point to latest.md for results and benchmark.md for methodology. Fix the wrong "(default)" label on `make bench`: the Makefile defaults to native-git.
- **Other docs.**
  - The dated measurement paragraph in `sql-m2.md` moves to history.
  - `guide.md`'s "~5 ms per transaction" becomes the mechanism (one fsync per transaction) plus a link to latest.
  - `working-state.md` keeps its design rationale ("exceeded 10 ms"), which now cites the M4.3 report.
  - `plan.md` is itself a historical planning record: its links point to history, and its narrative numbers stay.

## Decisions

- **Keep the name `docs/benchmark.md` for methodology.** It is linked from the README, and its opening already calls itself the entry point. Only its results sections move out.
- **No tooling in this issue.** Rendering latest.md from the JSON automatically would make refreshes mechanical. Propose it in rdb-6ae82d if the manual refresh proves error-prone.
- **Out of scope:** re-measuring (rdb-6ae82d, after rdb-bc42f6).

## Acceptance criteria

- No document outside `docs/benchmarks/` presents measured results, apart from:
  - the README table, captioned with its date and linking latest.md;
  - `plan.md`'s planning narrative;
  - the cited design rationale in `working-state.md`.
- Every history file starts with the banner, and all relative links and anchors resolve (checked with a script over README and all `docs/**/*.md`).
- The benchmark.md publishing process is explicit enough to follow without asking.
- `make lint`/`make test` remain green (docs-only change; sanity).
