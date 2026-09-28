# Walkthrough: scorecard refresh at c1aa399

## What was published

- `docs/benchmarks/latest.md` and `docs/benchmarks/latest/*.json`: the four-way scorecard (suite v1, default config) measured at `c1aa399` on a clean tree.
- The previous report moved to `docs/benchmarks/history/2026-09-28-57a0cd8-scorecard.md`, with its JSONs alongside. Only the history banner and relative links changed.
- The README performance table and prose, plus the README scope sentence, now say "local-first, version-controlled relational workloads" instead of "small tool databases", with every claim bounded to the measured sizes.

## How the run was controlled

- It used the same machine, OS, toolchain, Docker images and commands as the `57a0cd8` run, which makes the two directly comparable.
- The run came from a detached worktree of `c1aa399` in the scratchpad, with one `dbbench` binary built with `-tags gms_pure_go`.
- Runs were sequential with only the system under test up: native-git 11m49s, journal 9m09s (50 min previously), MySQL 10.6 s, Dolt 13 s.
- The Dolt container's earlier "Exited (1)" status was just `docker stop` interrupting the server, not a crash.

## How latest.md was written

- **Rendered tables.** A render script generates the tables from the JSON. Before use, it was validated by reproducing the old latest.md tables byte for byte. Two formatting edge cases surfaced: a value that rounds up to the next decade (0.998 → "1.0") and double rounding. After publishing, the tables were re-diffed against the JSON.
- **Noise control.** MySQL and Dolt didn't change between runs, so they measure noise: their small p50s moved by up to 1.7×. "Changes since `57a0cd8`" lists only RepoDB changes far beyond that. For example, the journal Update x100 change (7.0 → 9.0 ms) is not claimed, because MySQL's own Update x100 moved from 44 to 57 ms.
- **Attribution.** Changes are credited only where the mechanism is clear:
  - range speedups to key pushdown (rdb-586f81);
  - scan speedups to streaming plus binary rows (rdb-586f81 and rdb-ee17d5);
  - journal sync speedups to lazy replay (rdb-92b9e2), which the rdb-6ae82d profiling had identified as the cause.
- **Ordered limit.** The dbbench query is `ORDER BY id DESC LIMIT 20`, and reverse scans aren't implemented (rdb-acd36d). It therefore still sorts the whole table (37–39 ms), and latest.md says so rather than presenting it as unexplained.

## Corrections made during review

- **Conflict scope.** The README and latest.md said writers to *the same rows* are rejected. In both modes, any write transaction whose base is no longer the head is rejected, even if the rows are disjoint (the generation check in `working.go`). The mixed workload proves it: each client writes its own row, yet 6–15% of attempts are rejected. The text was fixed, and the follow-up work is epic rdb-df092b (per-key conflicts, group commit, safe autocommit retry, MySQL error codes).
- **Claims not made.** Checking raw samples ruled out two tempting headlines:
  - The native-git Update x1 improvement (319 → 196 ms) is a ~1.8 s stall that moved from Update x1 to Update x10. It is likely Git auto-gc; rdb-2124bc is filed to confirm it and keep gc out of timed workloads.
  - The journal "initial publish regression" (6.6 → 7.4 s) is a single sample per size, and it moved in both directions across sizes.
- **Qualified claims.** "Full scans competitive with MySQL" is stated with the caveat that external numbers include transferring all rows over TCP. "Batch writes under 10 ms" is qualified to journal mode.

## Follow-ups

- rdb-df092b (epic): local write coordination; now the main performance gap.
- rdb-2124bc: auto-gc in dbbench fixtures.
- rdb-59c7d7: the journal merge regression it tracks (19.8 s) now measures 2.6 s. It is left open for the owner to close.
