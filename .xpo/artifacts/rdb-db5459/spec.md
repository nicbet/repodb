# rdb-db5459: Workload profiling — append-heavy and mutable-data shapes

## What

Add two application-shaped workload groups to `dbbench` (`experiments/dbbench`), alongside the existing suite:

- **`append`** — agent traces / event logs: high-volume inserts, time-range scans, rare updates.
- **`mutable`** — issue tracker / board: frequent updates to existing rows, point reads, filtered queries.

Each group reports commit latency, query latency, memory (allocations and heap per measurement; process peak RSS), repository size growth over time, and sync cost, at 1k / 10k / 100k rows. The deliverable is both the harness change and a findings comment on this issue that ranks where optimisation effort should go (tree mutation, caching, compaction, sync).

## Why

Suite v1 measures generic primitives on a 2-column table with 32-byte values. It explicitly does not cover "realistic board requests" or growth over history (benchmark.md § Explicit coverage limits). Before the large epics we need numbers for the two shapes consumers actually run.

## How

### Placement

`benchmark.md` says to extend the one runner rather than add another headline benchmark. So: the same binary, report format and `measure()` path, with a new flag `-workloads core,append,mutable` (default: all three). The suite version becomes **2**, because the default run now contains new workloads. v1 rows are unchanged, so diagnostic comparisons of `core` rows against v1 remain meaningful, but published v1 and v2 scorecards are never compared as matched results.

### Shared workload code (RepoDB and external)

`external.go` currently duplicates the v1 workload code. The new groups are written once against a small interface:

```go
type sqlTarget interface {
    exec(q string, args ...any) error
    query(q string, args ...any) (rows int, err error)
    begin() (sqlTx, error)              // exec/query/commit/rollback
    sync() error                        // ErrUnsupported in external mode
    size() (int64, error)               // fixture bytes; external: data-dir size not available → omitted
}
```

RepoDB uses `engine.Session`; external uses `database/sql`. Sync/peer workloads are skipped in external mode, as they are today. The v1 code is **not** refactored in this issue.

### Fixtures (deterministic, fixed seed)

**append** — `events`:
```sql
CREATE TABLE events (
  id BIGINT PRIMARY KEY,          -- monotonic, so appends land at the right edge
  ts BIGINT NOT NULL,             -- ms, monotonic with small jitter
  session_id BIGINT NOT NULL,     -- 1 of rows/100 sessions
  kind VARCHAR(32) NOT NULL,      -- 8 kinds
  payload TEXT NOT NULL,          -- ~256 B, JSON-like
  INDEX by_session (session_id, ts),
  INDEX by_ts (ts)
)
```

**mutable** — `issues` plus `comments`:
```sql
CREATE TABLE issues (
  id BIGINT PRIMARY KEY, status VARCHAR(16) NOT NULL, assignee VARCHAR(32),
  priority INT NOT NULL, title VARCHAR(120) NOT NULL, body TEXT NOT NULL, -- ~1 KB
  updated_at BIGINT NOT NULL,
  INDEX by_status (status, priority), INDEX by_assignee (assignee, status)
)
CREATE TABLE comments (id BIGINT PRIMARY KEY, issue_id BIGINT NOT NULL, body TEXT NOT NULL, INDEX by_issue (issue_id))
```
Distribution: 5 statuses, 20 assignees. Update targets are Zipf-skewed (s≈1.1, which puts roughly 80% of updates on 20% of issues), because boards have hot rows.

The size tier `N` is the preloaded row count (events or issues; comments = 2N). Bulk load goes through 500-row multi-value INSERTs in one transaction, then the initial publication, exactly as in v1.

### Workloads (per size, after load)

**append**
| name | request |
|---|---|
| `append_single` | autocommit INSERT of one event |
| `append_batch_100` | transaction of 100 events |
| `append_concurrent` | 1/4/16 clients appending single events (disjoint id ranges) |
| `events_recent` | `WHERE ts >= ? ORDER BY ts DESC LIMIT 100` (last window) |
| `events_session` | `WHERE session_id = ? ORDER BY ts` (≈100 rows, via index) |
| `events_kind_count` | `SELECT kind, COUNT(*) … WHERE ts >= ? GROUP BY kind` (last 10%) |
| `event_update_rare` | 1 update per 100 appends, interleaved (reported separately) |

**mutable**
| name | request |
|---|---|
| `issue_point_read` | by id (Zipf) |
| `issue_board_query` | `WHERE status = ? ORDER BY priority LIMIT 50` |
| `issue_assignee_query` | `WHERE assignee = ? AND status <> 'done'` |
| `issue_update_status` | autocommit status + updated_at change (Zipf) |
| `issue_update_edit` | transaction: rewrite title + body (~1 KB) |
| `issue_comment` | transaction: INSERT comment + bump issue updated_at |
| `issue_mixed_concurrent` | 1/4/16 clients, 70% reads / 30% writes over the above; conflicts counted as in v1 |

### Growth over time and sync cost

Each group ends with a **growth phase** of `G` rounds (default 10). Each round runs a fixed write burst (append: 1 000 events in 100-event transactions; mutable: 500 Zipf updates + 100 comments), then times `sync`, then records:
- `fixture_file_bytes` (absolute, local + remote) and the delta;
- the `sync` duration for that round;
- for journal mode, the journal size before/after the sync checkpoint.

These are stored as a time series: a new `series` array in the report with `{group, rows, round, bytes, sync_ms, journal_bytes}`. The table prints the first, middle and last rounds; JSON keeps everything. Then, as in v1, a peer is cloned once and `peer_pull_after_growth` times the peer's sync of the whole accumulated history.

Git housekeeping stays manual: run `gitGC` untimed before the growth phase only, **not between rounds**. The phase measures unmaintained growth, which is what compaction work would address. The report already records `git_gc: manual`.

### Correctness checks

Final row counts (events, issues, comments), the last-written value of a hot issue, `events_recent` returning the expected newest id, and peer convergence (row counts and a checksum query) after `peer_pull_after_growth`. Failures mark the workload FAIL, following v1's rules.

### Docs

- `docs/benchmark.md`: workload contract version 2 (new groups, fixtures, growth series, `-workloads`), with the coverage-limits paragraph updated.
- `docs/testing.md`, if it lists dbbench flags.
- No `latest.md` change in this issue (decision 3).

## Acceptance criteria

- [ ] `dbbench -workloads append,mutable` runs in native-git, journal and external modes; external skips sync and growth sync.
- [ ] The JSON report has suite_version 2, all new measurements, and the `series` growth data. The printed table has a section for each group.
- [ ] Unit tests: deterministic fixture generation (same seed → same rows), the Zipf picker's bounds, the series record shape, and `-workloads` parsing/validation.
- [ ] A smoke run (`-rows 1000 -requests 5`) passes in all three modes in Docker.
- [ ] Reference runs at 1k/10k/100k in Docker, at one clean commit, for journal, native-git, MySQL and Dolt. The findings comment on this issue contains summary tables and a ranked list of bottlenecks, each with the evidence behind it (a profile where one was taken, in a Linux container).
- [ ] `docs/benchmark.md` describes v2.
- [ ] `go test ./...` passes.

## Decisions (confirmed with user)

1. **Sizes.** The new groups get their own flag, `-workload-rows`, defaulting to 1000,10000,100000. `-rows` (v1 core) keeps 1k/10k/50k.
2. **Baselines.** MySQL and Dolt also run the new groups (shared code; sync and growth-sync are skipped in external mode).
3. **Publishing.** Reference results go only in this issue's findings comment. `latest.md` waits for the next scorecard refresh.
4. **Growth rounds.** G=10 rounds per size is enough. A deeper history-depth study would be a separate issue if one is wanted.

## Implementation notes (decisions made while building)

- **Names.** The workload names are the ones in the tables above, unprefixed. Each measurement also carries `group` in JSON. Setup steps that both groups share are prefixed: `<group>_open`, `<group>_schema`, `<group>_bulk_load`, `<group>_initial_publish_sync`, `<group>_peer_pull_after_growth`. `core` rows have no `group`.
- **Flags.** `-workloads` (default `core,append,mutable`), `-workload-rows` (default `1000,10000,100000`, minimum 1000 so the board query's `LIMIT 50` and the per-session counts hold), and `-growth-rounds` (default 10). All three are recorded in the report's `config`.
- **Rare updates.** `event_update_rare` is its own measured workload: autocommit payload rewrites of uniformly random old events. Interleaving one update per 100 appends happens in the growth bursts (1 update after each 100-event transaction), where the ratio means something. A 30-request workload would never reach a 100th append.
- **Hot rows.** Zipf skews toward the **newest** issues (`id = n − zipf`), because on a board the recently filed issues are the hot ones. This also means hot writes cluster at the right edge of the tree. With s=1.1 the skew is stronger than 80/20: the newest fifth gets 84% of draws at 1k issues and 93% at 100k, and at 100k 75% of draws fall on the newest 1%. `docs/benchmark.md` gives the measured figures.
- **Text.** Filler text is pseudo-prose drawn from a fixed vocabulary of about 80 words, not hex, so Git/zlib compression behaves like it does on real text.
- **Series point fields.** `group, rows, round, table_rows, write_requests, write_p50/p95/max_ms, sync_ms, fixture_file_bytes(+_delta), journal_bytes_before/after_sync, process_heap_bytes_at_end`. Fields that a backend can't measure are omitted: external mode has no sync, sizes or journal. Journal bytes are the size of the working-state directory.
- **External conflicts.** MySQL errors 1213 (deadlock; Dolt also uses it for serialization failures) and 1205 (lock wait timeout) are reported as `ErrConflict`, so the concurrent workloads count them as rejected attempts, as they do for RepoDB. Core's external code keeps its existing behaviour.
- **Reference runs.** These use `make bench-docker BENCH_ALLOW_DIRTY=1 … -workloads append,mutable`, from this issue's worktree (main + the harness change only; no engine changes). The work isn't committed until `merge`, so the reports record the dirty status. That's acceptable because the results go only in the findings comment and are not published. `core` is skipped in these runs because `latest.md` already covers it.
- **Profiling flags (added for the findings).** `-cpuprofile <file>` profiles the whole run, with samples labelled `workload=<name>` (plus `clients`) through `pprof.Do` in `measure()`, and `<group>_growth_burst` / `<group>_growth_sync` in the growth phase. `-pprof <addr>` serves `net/http/pprof` with mutex and block profiling turned on. Both are diagnostic only and are documented in `docs/benchmark.md`. They were added after the reference runs and don't change any workload.
