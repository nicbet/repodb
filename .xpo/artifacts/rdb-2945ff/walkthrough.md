# Walkthrough: scorecard at 6b76e87 and README refresh

## What was published

A new `docs/benchmarks/latest.md` and its four raw reports, measured at `6b76e87`, the end of the 2026-10-02 performance round:
- rdb-48f066: binary nodes, projection, row counts;
- rdb-a6a4d2 and rdb-24820a: shared overlays;
- rdb-acd36d: reverse scans;
- rdb-a0d510: mixed boundary hash and 64/256 chunks, format 6;
- rdb-6bd369: the `PendingRows.With` sort.

Also a top-to-bottom refresh of `README.md`.

## How it was measured

- **Clean commit.** `make bench-docker` for native-git, journal, MySQL and Dolt, run from this issue's freshly created worktree before any file was edited. HEAD was `6b76e87`, and every report records an empty `working_tree_status`.
- **Isolation.** Runs were sequential with one baseline container at a time. The unrelated `servaint-*` containers were stopped, with the user's approval, and left stopped.
- **Same environment.** The baseline image digests, Docker 29.8.1, kernel 7.0.14-linuxkit and macOS 15.8 on AC power all match the previous run, so the comparison is like for like.
- **Dolt** again failed the concurrent-increment correctness checks. Its `make` exit was 2, and it's marked ✗ as before.

## How latest.md was written

- **Tables.** The render script from earlier publishes wasn't kept, so it was rewritten. It was validated by reproducing the `636af3d` Results section byte for byte from that run's JSON, then run on the new reports. Number formatting is two decimals below 1, one below 10, then whole numbers with separators; a value that rounds into the next band takes that band's format.
- **Noise reference.** MySQL and Dolt are unchanged software. Their p50 ratios between runs were 0.92–1.34× (MySQL) and 0.85–1.14× (Dolt) for 80% of workloads, and 0.6–2.4× at the extremes. "Changes since `636af3d`" lists only RepoDB changes well beyond that, each with the issue responsible.
- **Summary bullets** were re-derived from the rendered tables and cross-checked number by number.
- **Flagged, not explained:** the journal harness's peak RSS rose 276 → 350 MiB (native Git fell 217 → 140 MiB). It's one sample.

## README refresh

Every claim was checked against the code or a run:
- the Quickstart was run literally on an empty `git init`;
- the CLI table was checked against `cmd/repodb/main.go` (`snapshot` is an obsolete stub and stays out);
- the sync prompt was confirmed in `cmd/repodb/main.go`;
- the API names were checked (`eng.Checkpoint` is a method, not `engine.Checkpoint`);
- the types were checked against `sql.md`.

Changes:
- **Status paragraph:** current numbers, and an explicit note that the storage format changes without migration during alpha. Formats 4, 5 and 6 all shipped on 2026-10-02.
- **Platforms:** what is tested (macOS), what runs the harness (Linux), and Windows' build-only state.
- **Performance:** the table now includes the descending ordered limit, plus prose matching the current numbers and native-Git write latency. The single-row write claim gives the measured ranges rather than "about 2×", because inserts and deletes are about 1.4×.
- **Architecture:** a new content-addressed-tables bullet, and a SQL scope paragraph that lists all stored types, with backlog IDs for AUTO_INCREMENT, foreign keys and views.
- **Roadmap:** today's work is moved to Done. "Now" cites rdb-2c93ca, rdb-db5459 and rdb-df092b.
