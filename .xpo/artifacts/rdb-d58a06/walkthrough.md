# rdb-d58a06 walkthrough: the scorecard runs in Docker, like for like

## Why

The write-gap analysis for rdb-d690aa showed the scorecard compared two different stacks. RepoDB ran on the macOS host (APFS, `F_FULLFSYNC` per journal commit, ~5 ms). MySQL and Dolt ran in Docker Desktop's VM, where a synchronous write costs ~0.16 ms and never fully flushes the host SSD. The user decided the scorecard publishes only like-for-like numbers: every system in a container on the same VM, with `docs/benchmark.md` describing the setup explicitly.

## What was built

- **`experiments/dbbench/Dockerfile`.** Built with `golang:1.27-trixie`, the toolchain of all earlier scorecards; run on `debian:trixie-slim` with Debian's Git (user decision, version recorded). A non-root `bench` user with a Git identity, since fixture repositories commit. `.dockerignore` keeps `.git`, `.xpo`, build outputs and `bench-out` out of the context.
- **`make bench-docker`.**
  - Refuses a dirty tree, because the image is built from the files on disk and would otherwise measure code that isn't the reported commit. `BENCH_ALLOW_DIRTY=1` overrides for diagnostics, and the report records the dirty status (user decision).
  - Creates the `repodb-bench` network and the `repodb-bench-fixtures` volume, empties the volume, and runs one mode.
  - The report is written to `BENCH_OUT`; `BENCH_SUFFIX` separates the MySQL and Dolt reports.
- **Report metadata.** New flags `-revision`, `-working-tree-status` and `-runtime`. `newReport` uses an override whenever its flag was *set*, so an empty, clean status from the host is recorded as clean, not re-detected inside a container with no checkout.
- **Baselines measured from inside the VM.** Not in the original spec: the harness now also runs in the container for `-mode external`, reaching `repodb-bench-mysql:3306` and `repodb-bench-dolt:3306` by name. The old host-side harness went through Docker Desktop's port forwarding, which added ~0.15–0.2 ms to every baseline request.
- **`docs/benchmark.md` Setup section:**
  - where each system runs;
  - storage (named volumes, ext4 in the VM disk);
  - what a flush means there;
  - each system's default durability;
  - no resource limits, one container at a time;
  - the recorded metadata.

  Host runs (`make bench`, `make bench-external`) are documented as diagnostics and never published.

## The refresh, done in this issue (user decision)

- The harness changes were committed first (`636af3d`), and all four modes were measured at that clean commit.
- The published results are a second commit, merged with **fast-forward**, so the measured revision exists on main.
- Native-git and journal each took ~2.5 min; on the host they took ~10 min.

## What the like-for-like numbers showed

The earlier picture, RepoDB much faster on reads and 5× slower on writes, was largely the setup:
- the read lead was port forwarding on the baselines' side;
- the write gap was the host's full SSD flush on RepoDB's side.

At 50k rows, like for like:
- journal single-row writes 0.48–0.56 ms against MySQL's 0.51–0.57 ms, and 100-row batches 3.0 ms against 8.4 ms;
- MySQL faster on point reads (0.04 against 0.07 ms), full scans (13 against 34 ms) and joins (2.6 against 45 ms).

latest.md's "Changes since `87dd885`" explains the setup change rather than comparing numbers across setups. The README performance section and scope sentence were regenerated from the JSON.

## Also fixed: rdb-43dc0e

A 71 MB macOS `dbbench` binary had been committed at the repo root (`e33870e`). It is removed and `/dbbench` is ignored; its blob stays in history.
