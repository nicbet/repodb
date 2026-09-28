# Refresh latest.md with a full run at one clean commit

## What

Measure the scorecard (suite v1, default config: 1k/10k/50k rows, 1/4/16 clients, 30 requests) in all four modes at `main` `e33870e`, which includes rdb-3745e6, rdb-e472aa, rdb-bc42f6, rdb-ec8ec2 and rdb-22f505. Publish it as `docs/benchmarks/latest.md` following benchmark.md § Publishing results.

## Run controls

- **Clean revision.** Run from a detached `git worktree` of `e33870e` in the scratchpad, so every JSON records that revision and an empty `working_tree_status`. The main checkout's untracked xpo artifacts can't leak in.
- **Sequential, one system under test at a time.** native-git, then journal, then MySQL 8.4.11, then Dolt 2.3.5. Both containers are stopped during the RepoDB runs, and only the container under test runs during an external run.
- **Fixtures** go to the default temp dir on the internal APFS SSD. latest.md records the machine, OS, filesystem, Go, Git and server versions.
- Build once with `-tags gms_pure_go`, and use the same binary for all four runs.

## Publishing

- Raw JSON goes to `docs/benchmarks/latest/scorecard-{native-git,journal,mysql,dolt}.json`.
- **Nothing to archive.** The interim latest.md holds no measured results (it only points to the 2026-09-13 history report), so it is replaced rather than moved to history. The rule in step 2 exists to preserve past *results*.
- latest.md tables follow the old report's structure:
  - single-client p50 latency (1k and 50k rows);
  - concurrency at 50k rows;
  - sync at 50k rows (RepoDB only);
  - plus a "correctness" section listing every FAIL row with its reason.
- **Dolt:** its numbers are reported, but rows that failed correctness (lost updates, serialization errors) are marked, not presented as comparable latency.
- **README table:** refreshed from latest.md with the new date. Its prose claims ("sub-millisecond point reads", "~5 ms writes", "batch writes beat MySQL and Dolt") are re-checked against the new numbers and reworded if they no longer hold.
- **Interpretation:** latest.md explains notable changes from the 2026-09-13 report only where the data shows them. Point-read latency after the rdb-bc42f6 stat check is one such case; it is not speculated about.

## Acceptance criteria

- The four JSONs record revision `e33870e…` and an empty `working_tree_status`, suite v1 and the default config. The external ones record `server_version`.
- latest.md states environment, commands and links, and every number in it traces to those JSONs.
- The README table matches latest.md.
- The link check passes; `make lint` passes.
