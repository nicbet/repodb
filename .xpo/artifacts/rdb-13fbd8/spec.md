# rdb-13fbd8: Scorecard refresh at 87dd885

## What

Re-measure the four-way scorecard (native-git, journal, MySQL, Dolt), suite v1 with the default config, at clean `87dd885`. Replace `docs/benchmarks/latest.md` and `docs/benchmarks/latest/*.json` in place, and update the README table, following `docs/benchmark.md` § Publishing results.

**History.** The first run, at `f944eee`, completed cleanly. It showed a native-git contended-increment regression bisected to rdb-e0c717. The user chose to fix first: rdb-162912, merged as `87dd885`. Then everything is re-run and the `87dd885` run is published. The `f944eee` JSONs stay in the scratchpad as diagnostics and aren't published.

## Run

- **Checkout and binary.** Run from the issue worktree, fast-forwarded to `87dd885` and clean, so every JSON records that revision with an empty `working_tree_status`. One `dbbench` built with `go build -tags gms_pure_go ./experiments/dbbench`.
- **Order:** native-git, journal, MySQL (`127.0.0.1:3306`), Dolt (`127.0.0.1:3336`), with fixtures in the default temp dir.
- **Isolation** (user's decision): the unrelated `servaint-*` containers are stopped. The script logs the running containers at each phase. The baselines are stopped during the RepoDB runs, and only one runs at a time otherwise.
- **Baselines:** the local images, not re-pulled, so MySQL 8.4.11 and Dolt 2.3.5 match `c1aa399`. Docker 29.8.1.
- **New in the JSON:** `git_gc: "manual"` (rdb-2124bc).

## Write-up

- **Tables.** Generated from the JSON by a render script that was validated by reproducing the `c1aa399` Results section exactly.
- **"Changes since `c1aa399`".** MySQL and Dolt are the noise reference; claim only changes well beyond it. Attribute only with a clear mechanism:
  - rdb-fb3d12: native-git writes and outliers, checkpoints;
  - rdb-2124bc: Git housekeeping in sync workloads;
  - rdb-4d677f: journal size;
  - rdb-e0c717 and rdb-162912: native-git contention.
- **Summary and README:** journal is the default (rdb-c56142), native-git is audit mode. Every README number is checked against the JSON by script.

## Acceptance

- All four JSONs record `87dd885` with a clean tree, suite v1 and the default config.
- latest.md, its JSONs and the README are updated; links resolve; `make lint` and `git diff --check` pass.
- rdb-c56142's last acceptance item (the README and summary reflect the default) is met.
