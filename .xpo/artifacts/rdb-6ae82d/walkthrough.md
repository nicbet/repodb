# Walkthrough: first clean latest.md

## What was published

- `docs/benchmarks/latest.md`: the first scorecard measured at a single clean commit (`57a0cd8`) across all four modes.
- `docs/benchmarks/latest/*.json`: the four raw reports.
- The README performance table and prose, refreshed from them.

## How the run was controlled

- A detached worktree of `57a0cd8` (so `working_tree_status` is empty in every JSON), and one `dbbench` binary built with `-tags gms_pure_go`.
- Sequential runs: native-git (14 min), journal (50 min), MySQL 8.4.11 (10 s), Dolt 2.3.5 (13 s). Only the system under test was running; unrelated containers on the machine were killed first. Desktop apps stayed open, as `latest.md` notes.
- Two harness problems surfaced and were fixed before the published run, because all four runs must share a revision:
  - rdb-22f505: record server versions, and list the reason for every FAIL row.
  - rdb-802f5d: auto-gc pruning objects during the size walk aborted every native-git sync workload. The 2026-09-13 run silently suffered from this too.
  The first attempt at `e33870e` was discarded.

## How latest.md was written

- The tables are generated from the JSON by a render script, validated first by reproducing the 2026-09-13 report's numbers exactly. The concurrency table was rebuilt to show p50, successful ops/s and the rejected share together, because RepoDB rejects conflicting writers without retrying, and p50 alone hides that.
- Every prose claim was checked against the tables. Two corrections were made while doing that:
  - The rejection range is 37–70%, not 43–70%.
  - The native-git read speed-up had been attributed to rdb-bc42f6, but the old native-git run predates M4.4 (`559c46a` < `864731d`). So "changes since 2026-09-13" is stated as measured, without attribution.
- Fairness: RepoDB is embedded, while MySQL and Dolt go over loopback TCP. The notes say so, and quote RepoDB's own MySQL-protocol point read (0.14 ms at 50k rows) as the like-for-like figure.
- Correctness: MySQL, journal and native-git passed every check. Dolt 2.3.5 lost acknowledged increments (120→40, 480→34) and hit serialization errors. Its rows are marked ✗, and its 16-client rows at 10k/50k are "not measured", because the harness stops a fixture at its first hard error.
- The README table was verified against the JSON by a script.

## What the run taught us (follow-up work)

Journal mode's 50 minutes is 99% sync; its SQL-only workloads take 28 s against 10–13 s for MySQL and Dolt. Journal sync latency grows about 4× over 30 syncs, while native-git stays flat.

A git-call probe traced this to journal replay. On a cold cache, `WorkingState.load` replays the whole never-truncated journal and loads a snapshot for every historical checkpoint record, only to discard all but the last. That's O(history) per replay, and quadratic over a run of syncs.

Follow-ups, in bang-for-buck order:
1. Lazy checkpoint snapshots in replay.
2. Journal compaction (rdb-515fae).
3. Fewer git subprocesses per sync.
4. Range/scan pushdown.

rdb-59c7d7 (the journal merge-sync regression versus 2026-09-13) is likely the same mechanism.
