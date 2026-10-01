# rdb-2124bc walkthrough: Git housekeeping kept out of timed dbbench workloads

## How the cause was found

The issue suspected that Git auto-gc was causing the multi-second native-git update outliers, and asked for that to be confirmed before changing anything. `GIT_TRACE2_EVENT`, set on the dbbench process, is inherited by every Git subprocess, RepoDB's included. One traced 50k native-git run (20k processes) answered the question:

- **The update outliers aren't gc.** RepoDB's native commit uses plumbing only (`hash-object -w`, `read-tree`, `update-index`, `write-tree`, `commit-tree`, `update-ref`), and plumbing never runs `gc --auto`. No housekeeping overlapped any update workload. The slow commits spent their time inside `hash-object` and `write-tree`, both about 10× slower in the same commit. That points to a transient system-wide write/fsync stall. Investigating it is rdb-c85855.
- **Maintenance did land in the sync workloads.** Porcelain `git fetch` (and receive-pack in the bare remotes) starts `maintenance run --auto` **detached**. `fetch` returns in ~30–80 ms while the child repacks for 100 ms–1.6 s, overlapping whatever is timed next. One run had 615 such processes.

The issue was rescoped with the user to the maintenance policy.

## The change (`experiments/dbbench/main.go`)

- **`configureFixture(path)`** sets `gc.auto=0`, `maintenance.auto=false` and `receive.autogc=false`, which disables both legacy auto-gc and the newer maintenance hook, in bare and non-bare repositories. It runs right after each `git init`: the size's bare remote, `newRepo` (local, peer, sync-pair clones), and the sync-pair remote before its seeding fetch. The policy lives in each fixture's config, so a retained fixture shows it, and RepoDB's own Git environment isn't altered.
- **`gitGC(paths...)`** runs `git gc --quiet` in the local repository and its remote at four untimed points: after `initial_publish_sync`, and before the concurrency, wire and sync groups. Without auto-maintenance, loose objects pile up over hundreds of native commits. Packing at fixed points keeps the object store in the same state from run to run and mode to mode. Running gc while the engine is open is safe: the engine's long-lived `cat-file --batch` re-scans packs when a loose object disappears, and published data stays reachable from `refs/repodb/data`.
- **`report.GitGC`** (`"git_gc": "manual"`) records the policy. External runs omit it.

## Evidence it works

- Traced smoke runs in both modes: 0 `maintenance` and 0 `gc --auto` processes, only the 8 explicit gc calls.
- Two full native-git 50k runs against the traced baseline. Sync workloads lost their housekeeping overlap: `publish_sync_after_writes` went from 1,192 ms to ~390 ms, and `edit_sync_roundtrip` p95 from 1,202 ms to ~860 ms. Run-to-run noise without housekeeping remains (for example `sync_unchanged` p50 at 131 vs 214 ms), and the update stalls persist as expected (rdb-c85855).

## Docs and tests

- `docs/benchmark.md` gains "Git housekeeping": the policy, why, and the remaining fsync-stall caveat. It also lists `git_gc` among the report fields.
- `TestFixturesDisableAutomaticGitHousekeeping` checks the three settings in a bare remote and a `newRepo` clone.
- `directoryBytes` keeps its tolerance for vanishing files (rdb-802f5d).
