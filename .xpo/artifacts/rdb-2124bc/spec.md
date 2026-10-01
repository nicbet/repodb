# rdb-2124bc: Keep Git auto-maintenance out of timed dbbench workloads

## Investigation (step 1, done)

Traced `dbbench -mode native-git -rows 50000` with `GIT_TRACE2_EVENT` (full evidence in the issue comment):
- **Update outliers are not gc.** No housekeeping ran during non-sync timed workloads; RepoDB's commit path is plumbing only. The stalls sit inside `hash-object -w` and `write-tree` (both ~10× slower in the same commit), which fits transient fsync stalls. Moved to rdb-c85855.
- **Sync workloads are affected.** `git fetch` and receive-pack start `maintenance run --auto` detached: 615 processes per run, 100 ms–1.6 s each, overlapping the next timed request.

User decision: rescope this issue to the maintenance policy, and file the I/O stalls separately.

## How

In `experiments/dbbench/main.go`:
- `configureFixture(path)` sets `gc.auto=0`, `maintenance.auto=false` and `receive.autogc=false`. It is called after every `git init`: the bare remote in `runSize`, `newRepo` (local, peer, sync-pair clones), and the sync-pair remote before its seeding fetch.
- `gitGC(paths...)` runs `git gc --quiet`, untimed, in the local repository and the remote: after `initial_publish_sync`, and before `concurrency`, `wire` and `syncWorkloads`. Sync-pair repositories are fresh and small, so they get no explicit gc.
- `report.GitGC` (`"git_gc"`) is set to `"manual"` (`gitGCPolicy`) for native-git and journal runs; external runs omit it.

`docs/benchmark.md` gets a "Git housekeeping" paragraph with the policy and why, plus a note that native-git write outliers can come from fsync stalls (rdb-c85855).

## Decisions

- **Per-repository config, not process-wide `GIT_CONFIG_*`.** The fixtures carry their policy, so `git config` in a retained fixture shows it, and RepoDB's own Git environment stays untouched.
- **Explicit gc at group boundaries.** Without auto-maintenance, loose objects accumulate over hundreds of native commits. Packing at fixed, untimed points keeps object-store state comparable across runs and modes.
- **Auto-maintenance as a user-facing cost** isn't measured. A dedicated workload can be added later if wanted.

## Acceptance

- A traced smoke run in both modes shows 0 `maintenance` and 0 `gc --auto` processes, only the explicit `gc` calls. Verified: 8 each, against 615 maintenance processes before.
- Two consecutive native-git 50k runs complete, with sync workloads free of housekeeping overlap.
- `make test` and `make lint` pass.
