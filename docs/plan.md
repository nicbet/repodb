# RepoDB implementation plan

Status: M0 through M4.1 complete; M4.2's bounded investigation is closed with
explicit follow-ups. M4.3 durable working state and intentional data commits is
in progress, before M5. Updated 2026-09-13. M4.3's section distinguishes its
implemented prototype from remaining target behavior.

## Product goal

RepoDB is shared database infrastructure for tools whose state belongs to an
existing Git repository. Issue trackers, kanban boards, and agent trace tools
can use SQL instead of each implementing object storage, history, transport,
and reconciliation themselves.

Code and database share a repository and remote, with independent histories.

## Agreed requirements

- Provide MySQL-compatible SQL and transactions within a documented scope, plus
  an optional MySQL wire-protocol server for existing drivers and tools.
- Support direct, in-process use as a Go module. The embedded engine must not
  require a server process, TCP connection, or automatic background service.
- Keep committed schema/data versions in Git objects reachable from dedicated
  RepoDB refs. M4.3 adds a durable local journal over a committed Git snapshot for
  current SQL working state. The journal is authoritative until checkpointed;
  it is not a disposable cache.
- Persist successful SQL transactions automatically. Users need not stage files,
  export tables, or manually create Git snapshots for ordinary persistence.
- Separate SQL transaction commits from intentional data-history commits.
  `repodb commit` records a shareable version; `repodb sync` exchanges committed
  history. Uncheckpointed SQL changes survive restart without either command.
- Keep database changes out of the source worktree, index, and code branch history.
- Support multiple applications and independent clones, including offline writes
  and synchronization without silent data loss.
- Make repository integration an explicit, idempotent `repodb enable` operation.
  A normal clone gets the code; enable discovers/fetches database refs and sets up
  their future transport. Enable does not start a server.
- Offer `repodb start` for users who want a standalone MySQL server. Embedded
  applications own the lifecycle of their engine instances.

Target workflow after M4.3 (`commit` and `diff` are not yet implemented):

```sh
# Regular git clone
git clone <project-url>
cd <project>

# Fetch database refs and configure future fetches; no hooks or service required
repodb enable --remote origin

# Optional: start the MySQL server; embedded applications open the engine directly
repodb start

# Review and checkpoint locally durable SQL changes when ready
repodb status
repodb diff
repodb commit -m "Triage issues"

# Explicitly synchronize committed data history with the selected remote
repodb sync --remote origin
```

Transparency means persistence and Git storage are handled by RepoDB. Genuine
concurrent editing conflicts still need an application policy or user resolution.
Local SQL success does not imply that an offline or failing remote has received it.

M0 established an explicit transport contract: enable adds a fetch refspec into
`refs/repodb/remotes/<remote>/data`, preserves existing configuration, and installs
neither push refspecs nor hooks. Ordinary `git push` does not publish database
changes; `repodb sync` is the guaranteed database network operation. Engine open
and transaction boundaries inspect local state without implicit network I/O.
M4.3 may add an opt-in pre-commit diagnostic; hooks remain outside durability
and sync correctness. The current [command matrix](git-integration.md) must be
updated when M4.3 is delivered.

## What exists today

| Component | Current implementation | Required change |
| --- | --- | --- |
| `server/` | MySQL listener routed through the persistent shared engine | Broaden compatibility after merge correctness |
| `client/` | MySQL driver wrapper | Retain as optional network client |
| `common/prolly/` | Deterministic bulk/streaming construction, canonical incremental mutation, lazy iteration, lookup, and graph validation | Reuse typed roots and incremental mutation in the working-state prototype |
| `common/storage/` | SHA-256-addressed store interface and memory/filesystem implementations | Retain interface; repository snapshots and writers provide Git-backed stores |
| `common/repository/` | Versioned snapshots, outcome recovery, graph validation, shared publication lock/CAS, immutable-object reuse, two-parent merge candidates, and legacy import | Define journal authority and recoverable Git checkpoint publication |
| `common/git/` | Batched blob reads/writes, classified expected-head updates, fsync, remote transport, and exact-candidate pushes | Attribute process, temporary-file, object I/O, and durability costs |
| `engine/` and `integration/` | Persistent SQL with edit overlays, streaming merging, root reuse, validation cache, explicit sync, and durable conflicts | Separate durable SQL saves from data commits and define clean-state sync |
| CLI | `init`, `status`, `import-legacy`, `enable`, `sync`, `conflicts`, `resolve`, `start`, and network `sql` | Design explicit data commits and working-state inspection |

Existing tests cover tree determinism and validation, persistent embedded and
MySQL SQL, restart recovery, publication outcomes through both interfaces,
separate-process and linked-worktree contention, injected failures, Git
SHA-1/SHA-256 formats, legacy import, enable/sync idempotence, fast-forward
transport, pinned transactions, disjoint offline merging, durable row/schema
conflicts, explicit resolution, and repeat-sync idempotence. `make test` passes.
Broader compatibility and the M4.3 working-state model remain pending.

The later M4 benchmark passed engine and integration tests, but its restricted
environment could not bind a loopback socket for the full MySQL wire suite. Its
results establish merge costs, not an additional full-suite validation or a
larger supported workload. Keep the subsequent M4.1 acceptance record separate
from M4.2 performance measurements and validation.

The tracked-directory design remains superseded. M4.3 restores intentional
data-history commits over automatically durable SQL state; it does not restore
tracked `.repodb/` files or require snapshots to save work.

## Architecture and remaining design work

### One engine, two entry points

Introduce an `engine` package responsible for repository-backed catalogs,
sessions, queries, transactions, and lifecycle. Applications use this package
directly; `server` adapts it to the MySQL wire protocol. Both paths must exercise
the same persistence and transaction code.

Keep repository storage, Git transport, and SQL concerns behind separate package
boundaries in the existing Go module. Define a small typed embedded API for
open/close, sessions, execution, result iteration, and transactions. Do not require
callers to depend on internal `go-mysql-server` types. Exact signatures remain open.

### Git representation and transaction publication

Start with one versioned catalog containing application databases/namespaces and
one local head, `refs/repodb/data`, as established by M0/M1. Transactions across namespaces
can therefore publish atomically. Namespaces provide organization, not access
control between applications with filesystem access to the same repository.

Each data commit points to a Git tree containing a versioned manifest and every
blob needed to read that snapshot. Preserve the current content hashes initially
by mapping them to Git tree entries; Git object IDs and RepoDB SHA-256 hashes are
different identities and must not be conflated.

**A hash inside a JSON blob is not a Git reachability edge.** Every schema/data
chunk must also be reachable through actual Git trees, otherwise ordinary object
transport and garbage collection cannot preserve the database. Avoid depending
on loose object files or a warm local cache. Publish through Git plumbing without
checking out the data tree or modifying the user's index.

Implemented baseline through M4.2: each successful write transaction creates one
internal data commit and atomically advances the data ref using its expected old
value. Autocommit statements follow the same path. Explicit rollback publishes
nothing. Unchanged transactions need not create new commits. This deliberately
replaces the prototype's rule against automatic Git commits; these commits live
in independent database history. M4.3 deliberately supersedes this coupling:
SQL commits advance durable local working state, while explicit data commits
advance Git history. Existing storage documentation describes the implemented
baseline until the new model is delivered.

Persist objects before publishing the ref and acknowledge success only after the
required durability steps. An interrupted write leaves either the previous state
or a complete new state. Record filesystem/fsync and Git-version assumptions;
atomic ref replacement alone is not a complete power-loss durability guarantee.
M4.2 measured this baseline and identified object/tree hardening as a dominant
cost. M4.3 now evaluates the explicitly accepted separation of acknowledged
local writes from Git-owned committed versions.

M2 distinguishes definite rejection, successful publication, and unknown commit
outcomes, including recovery through embedded and MySQL interfaces. Preserve this
contract during optimization: a post-publication verification error must not
imply rollback or authorize automatic replay.

M2 derives inventories from reachable catalog objects, batches Git reads, and
reuses existing Git object IDs. Historical commits retain older snapshots.
M4.1 adds merge root reuse, streaming reconciliation, reuse of loaded snapshots,
a bounded SQL validation cache, and batched object writes. M4.2 adds point lookup,
edit overlays, incremental mutation, publication reuse, and shorter sync locks.
Remaining chunk/tree amplification is measured in [performance.md](performance.md).
M4.3 preserves automatic SQL durability and committed snapshot reachability while
changing the local persistence mechanism.

### Local concurrency and remote concurrency

Discover the common Git directory, including linked worktrees, instead of assuming
`.git` is a directory. Repository-wide state and locking belong to that shared
identity. Readers pin immutable snapshots; publication is serialized across
processes and checked against the expected data head. A stale writer must receive
a defined retryable conflict or pass explicit validation, never overwrite a newer
head. Do not transparently replay arbitrary application transactions.

M4.3 SQL writers must compare durable working generations, not just Git heads:
multiple transactions can change the journal without making a data commit.
Linked worktrees initially share one repository-wide working database. Data
commits and sync coordinate both the journal state and the committed Git head.

Independent clones do not share locks. Fetch remote heads into a separate tracking
namespace (`refs/repodb/remotes/<remote>/data`, established by M0) rather than replacing
the writable local head. Reconciliation uses a common ancestor and produces a
validated merge commit before publishing. Remote races require bounded retries.
Do not force-push to resolve divergence.

The initial merge policy should accept unchanged/one-sided changes and disjoint
row edits, and report competing edits to the same row conservatively. Conflicting
DDL and constraint violations block publication. Persist or reproducibly derive
conflict details without making an invalid merged catalog the live database.
Provide inspection/resolution APIs before adding application-specific policies
for operations such as append-only events or independent field edits.

## Implementation sequence

### M0 — Prove Git storage and integration constraints

**Status: complete (2026-09-11).** The executable experiment is in
`experiments/gitstorage`; the resulting ref, transport, synchronization, session,
and hook decisions are documented in `docs/git-integration.md`.

Build small integration experiments using a bare remote and two local clones.
Use synthetic database manifests; do not wait for a complete SQL engine.

- Prove a custom data ref can carry all required objects through push/fetch and
  survive repacking, garbage collection, and cache deletion.
- Exercise compare-and-swap publication and discovery from linked worktrees.
- Prototype enable-time fetch/push configuration and hook composition. Test plain
  push, explicit branch pushes, fetch, pull with merge/rebase, offline operation,
  pre-existing refspecs/hooks, and no source changes pending.
- Decide when fetched database state is reconciled: on engine open/transaction
  boundaries, an explicit sync call, or a supported hook. Existing sessions must
  retain their transaction snapshots and observe updates at a defined boundary.
- Record the supported ordinary-Git command matrix and how partial code/data
  synchronization is reported. Do not imply atomic publication of both histories
  unless the selected transport actually provides it.

Git refspecs configure transport, not database merging. Git has no general
`post-fetch` hook; `post-merge` does not cover every fetch/pull path. A `pre-push`
hook can inspect or reject a push, but cannot simply add refs to its parent's
already selected update list. Explicit refspecs can override configured defaults.
These constraints must shape the implementation, not be hidden by an enable flag.

**Exit:** executable experiments plus a documented integration decision. If full
transparency needs a narrower command contract, make that product tradeoff explicit
before implementing the integration. Do not silently redefine ordinary Git behavior.

### M1 — Implement durable repository snapshots

**Status: complete (2026-09-11).** The repository API, snapshot writer and reader,
integrity validation, process lock and CAS publication, legacy import, and
cross-clone reconstruction are implemented and documented in
`docs/storage-format.md`.

- Replace tracked `.repodb/` authority with versioned Git trees and dedicated refs.
- Add complete snapshot read/write, integrity checks, atomic ref publication, and
  process-safe coordination. Use the existing narrow Git CLI boundary initially.
- Define format versioning, commit metadata/identity, local cache locations, and
  behavior for absent/corrupt/unsupported repositories.
- Provide an explicit import path or documented reset path for the experimental
  `.repodb/` layout; never silently delete it.

**Exit:** publish a synthetic catalog, close/reopen, delete caches, transfer it to
a second clone, and reconstruct it exactly. Failures during publication cannot
expose partial state; source HEAD/index/worktree remain unchanged.

### M2 — Deliver persistent embedded SQL and the shared server

**Status: complete (2026-09-12).** The persistent embedded engine, shared MySQL
server path, transaction outcome recovery, process/fault tests, and bounded
benchmark are implemented and documented in [docs/sql-m2.md](sql-m2.md).
The implemented SQL scope is deliberately narrow; the completed contract and
failure checks are listed below.

- Define definite failure, committed, and unknown-outcome behavior for repository
  publication and its embedded/MySQL callers. Avoid reporting post-publication
  verification failures as definite rollback; provide a way to inspect/recover
  uncertain outcomes without blindly replaying application transactions.
- Distinguish stale-head conflicts from lock contention, permission failures,
  filesystem errors, and cancellation. Do not map every Git "cannot lock ref"
  message to a retryable stale-writer conflict.
- Add separate-process writer contention tests, including linked worktrees, and
  fault injection before, during, and after ref publication. Verify complete
  old/new state, outcome reporting, and recovery after writer termination. These
  tests supplement the documented fsync assumptions; they do not prove power-loss
  behavior on every filesystem.
- Extract the engine and implement schema persistence and table adapters over
  Prolly roots, including scans and deterministic typed row/key encoding.
- Validate schema and Prolly object graphs, including descendant availability,
  before publication. Derive the current snapshot's object inventory from catalog
  roots so obsolete chunks need not remain in every subsequent snapshot.
- Start with explicit primary keys, a documented small type set, basic DDL/DML,
  queries, parameter binding, autocommit, explicit commit, and rollback. Report
  unsupported behavior clearly; SQL parser acceptance is not compatibility.
- Define isolation, statement failure behavior, and MySQL DDL transaction behavior.
  Implement snapshot readers and stale-writer detection before claiming durability.
- Route the existing server through the same engine and add explicit `repodb start`.
  Expose embedded open/close/query operations without opening a network listener.
- Use bulk rebuilds only as an explicitly bounded initial implementation. Add
  incremental updates when measurements show the cost; retain deterministic roots.
- Measure open/begin/commit latency, memory, object count, and Git subprocess count
  for repeated updates as well as inserts. Document the supported initial workload;
  prioritize object-ID reuse and batched reads if they dominate that workload.

**Exit:** an embedded program creates an issue table, commits rows, exits, and a
MySQL client reads the same rows after a server restart. Rollback and definitely
rejected writes leave the live state unchanged; uncertain commit outcomes are
reported explicitly and recoverably. No failure exposes a partial snapshot.
Separate-process tests demonstrate stale-writer rejection without lost writes.

### M3 — Enable and transport one history safely

**Status: complete (2026-09-12).** Idempotent explicit-remote enable, validated
tracking fetches, shared-lock fast-forward synchronization, divergence
preservation, CLI diagnostics, and pinned-transaction behavior are implemented
and documented in [docs/sync-m3.md](sync-m3.md).

- Implement idempotent `repodb enable` based on M0's proven integration, including
  existing-state discovery and an explicit empty-database initialization path.
- Preserve user hooks/configuration. Install no hooks or push refspecs in the enable flow.
  Select remotes deliberately; do not publish to every remote.
- Implement a reusable sync API and a diagnostic `repodb sync` command. Consumers
  and Git integration call the same implementation.
- Support initial transfer and fast-forward synchronization. Until M4, divergent
  histories produce an actionable error and preserve both sides.
- Report local durability, incoming/outgoing state, and transport failure separately.

**Exit:** clone, enable, write through either entry point, transport through the
supported Git workflow, and read from another enabled clone without manual snapshots.
Enabling twice neither duplicates configuration nor starts a process.

### M4 — Reconcile independently edited clones

**Status: functionally complete (2026-09-12).** Common-ancestor three-way merging, whole-row
conflicts, durable inspection/resolution, supported-constraint validation,
two-parent publication, and bounded local/remote race retries are implemented
and documented in [docs/merge-m4.md](merge-m4.md).

- Add row/schema diffs, three-way merge, conflict inspection and resolution, and
  validation of supported constraints before advancing the live data ref.
- Cover insert/insert collisions, update/update, update/delete, schema changes,
  and supported primary-key uniqueness. Secondary unique indexes remain M6 scope.
- Document distributed row identity: a local auto-increment counter alone cannot
  prevent independently generated IDs from colliding across clones.
- Handle remote advancement during synchronization without force updates or
  unbounded retries. Keep failed merges inspectable and repeatable after restart.

**Exit:** two offline clones add different issues/comments and converge; competing
edits surface as conflicts, survive restart, and converge after explicit resolution.
Repeat synchronization is idempotent and preserves both histories.

### M4.1 — Close the measured merge performance gaps

**Status: complete (2026-09-12).** This focused follow-up was completed before M5. Functional
M4 acceptance remains satisfied; the [benchmark](m4-bench.md) exposes avoidable
work that should be removed before expanding examples or workload claims.

**Baseline:** on the recorded Linux/amd64 system, a 50,000-row table with disjoint
1% edits on each side takes about 763 ms and allocates 288 MB. An unchanged table
takes about 555 ms and allocates 163 MB. These are medians of three runs covering
`BeginMerge` and `MergeSnapshots`, including snapshot loading. They exclude final
Git publication and network transport. Allocated bytes are cumulative allocation,
not peak resident memory. `BeginMerge` adds six Git processes by reloading local
and remote snapshots. Keep this baseline reproducible at 1,000, 10,000, and
50,000 rows; compare changes on the same machine and benchmark configuration.

Implement and measure in this order:

1. **Reuse selected immutable table roots.** When a selected table is already in
   the writer's local base, retain its schema/data roots and reachable inventory
   directly. For a remote-selected table, import only reachable objects missing
   locally. Avoid decoding and rebuilding rows merely to copy a table. Preserve
   validation of incoming data and complete Git reachability. Cover unchanged,
   one-sided, identical-change, and whole-table conflict-resolution selections,
   including multiple tables with only one changed.
2. **Reuse loaded snapshots and check equal heads early.** Let merge writers use
   the already validated immutable snapshots, tied to the correct repository and
   expected parent IDs, instead of reloading them. Keep the publication-time
   expected-head check. After fetch/ref resolution, detect equal heads before
   eager remote loading. Repeated up-to-date sync should perform no row decoding,
   rebuilding, or new commit creation. An equality shortcut must not substitute
   for validation when opening or adopting an unvalidated snapshot.
3. **Stream the ordered row merge.** Add lazy sorted Prolly iterators and a builder
   accepting sorted entries, then merge base/local/remote in primary-key order.
   Remove the three full row maps, all-key set, and repeated cloning/sorting from
   the merge path. Preserve canonical roots, whole-row conflict semantics, schema
   and constraint validation, cancellation, and iterator cleanup. Bound iterator
   and builder buffering; report snapshot/object buffers, output, and accumulated
   conflicts separately rather than claiming constant memory for the entire sync.
4. **Measure the complete synchronization path.** Add benchmarks with a bare
   remote and two clones covering unchanged sync, sparse edits, remote-selected
   tables, many unchanged tables, and conflict-heavy edits plus resolution. Include
   publication, fetch/push, retries, and repeated sync. Record wall time, allocated
   bytes, peak resident memory, Git processes, object writes, and repository growth;
   distinguish local-remote measurements from real network latency and warm-cache
   measurements from cold opens.
5. **Address the remaining measured I/O costs.** Profile bounded validation caching
   by repository identity, immutable commit ID, and validation/format version,
   plus batch Git object writes. Implement where the full-path measurements show
   material benefit, or record evidence for deferral to M6. A cache miss, eviction,
   or deletion must permit full reconstruction and validation from Git. Batched
   writes must preserve object verification, required fsync ordering, and explicit
   publication outcomes; this does not introduce delayed durability or a WAL.
   The focused APFS/Trace2 comparison found that the current `hash-object
   --stdin-paths` strategy does not batch loose-blob hardening, while `write-tree`
   changes from one full flush per new tree to one full flush plus writeout-only
   requests under `core.fsyncMethod=batch`. Keep `committed,reference` identical
   between variants. Treat tree batching, a genuinely batch-capable blob path,
   and reduced chunk/prefix-tree amplification as separate follow-up experiments;
   Trace2 counters do not replace power-loss qualification.

**Acceptance and validation:**

- Retained tables keep exactly the selected schema/data roots; copying an already
  available table performs no row rebuild or redundant blob write. Remote imports
  remain complete after cache deletion, transfer, and GC.
- Writer construction adds no duplicate local/remote snapshot loads or the six
  associated Git subprocesses when supplied validated snapshots. After fetch and
  ref resolution, an already validated equal-head sync performs no table scan;
  measure both this repeated path and cold validation explicitly.
- Streaming output matches the existing implementation's roots, rows, and conflict
  decisions on deterministic fixtures, including empty tables, insert/delete,
  conflicting updates, and schema selections. Report before/after sparse and
  unchanged results at all three sizes, with lower allocations attributable to
  removal of full-table intermediates. Establish measured workload limits rather
  than an arbitrary cross-machine millisecond threshold.
- Preserve pinned readers, stale-writer rejection, bounded local/remote retries,
  conflict persistence/resolution, and committed/rejected/unknown outcomes. Keep
  corruption tests for previously unseen incoming snapshots and cache-miss paths.
- Run `go test ./...`, `go test -race ./common/repository ./engine ./integration`,
  `make build`, and `git diff --check`, plus the benchmark command in
  [m4-bench.md](m4-bench.md). Run MySQL wire tests where loopback binding is allowed;
  do not count an environment-blocked run as a pass.

**Exit:** steps 1–3 are implemented with regression coverage; full-sync measurements
and before/after results are published in the benchmark documentation; step 5 has
an implementation or an evidence-backed M6 deferral. All correctness checks pass.
The existing small-database scope remains until the complete measurements support
a documented change; a 50,000-row merge microbenchmark alone does not broaden it.

### M4.2 — Establish database performance and remove the largest avoidable costs

**Status: closed as a bounded investigation (2026-09-12).** Results, incremental
writes, and the paired Git hardening audit are in [performance.md](performance.md).
This is an explicit scope disposition, not a claim that the full matrix ran.
M4.3 takes durable-save, realistic-read, and recovery comparisons; M5 takes
application/wire/CLI and contention coverage; M6 takes broader payload, history,
packing, network, and memory characterization. The matrix and original exit
criteria below remain as the historical scope.

The focused exact-key experiment is complete: transaction overlays and canonical
incremental Prolly mutation eliminate unrelated row decoding, and publication
reuses verified blobs and updates the parent Git tree. Median one-row writes were
166 ms, 864 ms, and 968 ms at 1,000, 10,000, and 50,000 rows respectively on the
recorded M4.2 machine. The 10,000-to-50,000 increase is no longer proportional to
row count; Git object/tree durability dominates after chunk-boundary propagation.
This supports continuing the architecture while retaining its current write-
latency and small-database limits.

**Evidence and limits:** M4.1 reports 160 ms and 120 MB allocated for a sparse
50,000-row merge, versus 6.09 s and 371 MB for full local-remote sync. An up-to-date
1,000-row sync takes 160 ms with 17 Git invocations; sparse sync uses 42 invocations
at every measured size. Trace the complete path before attributing its cost to
process startup, fsync, or merging. The microbenchmark and sync differ in scope
and cache state; subtracting their times is not an exact phase breakdown.

The original baseline used Linux/amd64; M4.1 used Darwin/arm64 and moved snapshot
loading outside the merge microbenchmark. Neither timing nor allocation ratios
are a controlled same-scope comparison. Full-sync numbers are single-operation
warm-cache samples, not latency distributions. Peak RSS includes fixture creation
and excludes Git child-process memory. The many-table fixture has 49 one-row
tables, remote selection adds a one-row table, and the conflict case has one
conflicting row. Extend these before making broader workload claims.

**Baseline deliverable:** a reproducible runner and `docs/performance.md`, with
raw results, machine/OS/filesystem, Go/Git versions, revision and working-tree
changes, durability settings, and fixture seeds. Start with a representative
matrix, then vary dimensions independently rather than testing every combination.

| Dimension | Required coverage |
| --- | --- |
| Data | 1,000 / 10,000 / 50,000 rows; narrow rows and larger payloads; one table and multiple populated tables |
| Reads | Cold open, warm transaction start, primary-key hit/miss, limited range/list query, full scan with consumed results |
| Writes | Single insert/update/delete, unchanged update, failed statement/rollback, explicit batches of 1 / 10 / 100 / 1,000 mutations |
| Applications | Append-heavy events and mutable issues; embedded API and persistent MySQL connection; CLI startup measured separately |
| Concurrency | Readers with 1 / 2 / 4 writers; slow sync overlapping SQL commits; successful throughput, rejection rate, retries, and latency |
| Sync | Equal heads, sparse divergence, large selected/unchanged tables, multiple conflict fractions and resolution, controlled remote latency |
| Lifecycle | Fresh process versus warm engine; first object creation versus reuse; loose versus packed objects; longer history at fixed live row count |

Treat larger cases as characterization, not supported capacity. Repeated fixture
resets must not silently turn new-publication tests into reuse of objects already
created by earlier iterations. Distinguish process-cold from filesystem-cache-cold
runs. Collect repeated independent samples and per-operation p50/p95; report p99
only with enough samples to support it. Record cumulative allocations, live heap,
and operation memory separately. Prepare fixtures outside a fresh measured process
for RSS comparisons and include Git children where possible; label unavailable
measurements. Record repository growth and transmitted bytes separately.

**Attribution:** time SQL analysis/execution, snapshot loading, validation, codecs,
tree construction, object writes, publication lock wait/hold, ref update, and
transport. Count decoded rows, visited/reused nodes, bytes read/copied/written,
and actual newly created objects separately from attempted blob writes. Use Go
CPU/allocation profiles and execution traces, Git Trace2, and targeted OS I/O
tracing where needed. Keep profiler runs separate from headline timings.

**Ranked optimization hypotheses from the current implementation:**

1. **Make SQL work proportional to touched data.** `StartTransaction` loads every
   table, `StatementBegin` clones a whole table, and a dirty `CommitTransaction`
   rebuilds all tables. Evaluate per-table dirty/root tracking and undo records for
   touched keys first, preserving failed-statement rollback and no-op behavior.
   Then evaluate lazy table loading and a transaction overlay of edits/deletions
   over immutable roots. Reuse unchanged roots and M4.1 iterators/builders before
   implementing incremental rechunking. Snapshot loading itself must also become
   lazy or reused to remove database-size cost from warm point reads.
2. **Expose primary-key lookup to SQL.** Prolly point lookup exists, but the SQL
   adapter scans through `PartitionRows`, which sorts and clones all rows. Evaluate
   equality lookup pushdown and lazy result iteration, verified through query
   plans and node/row counters. Preserve type/collation semantics. The current
   length-prefixed textual key encoding is not generally SQL-order-preserving;
   range pushdown requires a separate ordering design or explicit supported subset.
3. **Keep network latency out of local publication locks.** `pushExpected` holds
   the publication lock throughout `git push`. Evaluate capturing/checking the
   immutable candidate under lock and pushing that exact candidate after release.
   A concurrent local descendant must remain outgoing work; remote races still
   require bounded reconciliation and accurate status. Preserve candidate
   reachability. Separately profile moving immutable commit preparation outside
   the lock, accounting for GC and crash recovery. Keep expected-head checks and
   repository-wide conflict semantics; shortening locks does not eliminate stale
   transaction conflicts.
4. **Remove object-buffer copies before changing codecs.** `ReadObjects` buffers
   the full batch output before parsing blobs, and publication copies retained
   bytes. Evaluate streaming pipes, explicit immutable buffer ownership, lazy
   object access, and byte-budgeted caches. The current 128-entry validation cache
   bounds entry count, not bytes. Do not expose mutable shared buffers or make a
   cache authoritative. A persistent Git reader may benefit a long-lived embedded
   engine without requiring a daemon for CLI users.
5. **Exploit equal hashes below the table root.** If sparse traversal remains
   significant, explore skipping equal Prolly subtrees in merges and updates.
   Align key intervals when chunk boundaries differ; measure nodes visited versus
   changed key ranges without promising O(changes) for arbitrary rechunking.
6. **Separate process batching from durable I/O batching.** One `hash-object`
   invocation still creates temporary inputs and writes loose objects under the
   configured fsync policy. Measure temporary-file, object, tree/index, and fsync
   costs before considering pack-oriented ingestion or reuse of Git subtrees.
   Lower process counts alone do not prove lower disk costs. WALs, group commit,
   binary-format changes, and a replacement Git backend require separate proposals.

**Implementation scope and exit:** establish the baseline first, then select and
implement the two or three largest avoidable costs supported by profiles. Measure
each independently and publish before/after results, raw samples, and profiles.
Add structural regression checks for unrelated rows decoded, tables rebuilt,
bytes copied, and lock scope alongside repeatable performance checks. Run the full
tests, race tests, build, and diff checks from M4.1; retain publication, corruption,
isolation, merge, and recovery coverage. Document a practical workload envelope
and latency expectations. Rank remaining ideas by evidence, benefit, and risk for
M5/M6 instead of opening an unbounded series of performance milestones. Proceed
to M5 to validate these assumptions with real applications.

### M4.3 — Design and benchmark durable working state and intentional data commits

**Status: in progress (2026-09-13).** The design, opt-in chunk-journal/checkpoint
prototype, incremental replay, phase attribution, and typed-edit lower-bound
experiment are implemented; a production typed-edit overlay, full benchmark,
realistic fixture, and hardening matrix remain pending. Continue comparing
journal-backed SQL with M4.2's native-Git publication path. Use the
results to make an explicit adoption decision before changing defaults. This
milestone plans and tests the new persistence model; it does not assume its
latency or production readiness.

**Target product contract:**

| Operation | Meaning |
| --- | --- |
| SQL autocommit / `COMMIT` | Durably save the transaction in the local journal and publish its working generation |
| SQL `ROLLBACK` | Discard that transaction's edits, preserving previous durable working changes |
| `repodb status` / `diff` | Inspect changes since the last data commit, separately from incoming/outgoing committed history |
| `repodb commit -m "..."` | Capture a consistent durable working generation as a Git snapshot and conditionally advance `refs/repodb/data` |
| `repodb sync --remote ...` | Exchange committed history without silently committing or overwriting working changes |
| Optional pre-commit diagnostic | Report pending data changes and how to commit them; warn or refuse the code commit according to explicit policy |

SQL saves require no checkpoint, hook, network, or background service. Data
commits initially capture the whole catalog, not individual staged rows. They
record selected versions rather than every intermediate SQL transaction in Git.
Row staging, stash/reapply, and separate per-worktree databases are follow-ups.

**Design deliverable:** `docs/working-state.md`, covering:

1. **State and authority.** Distinguish committed Git head, durable working
   generation/root, and transaction-local edits. Keep journal and recovery metadata
   beneath the common Git directory, outside source files and the index. Reuse
   existing typed encodings and Prolly roots where practical. A clone reconstructs
   committed data; newer local changes require the journal as well. Specify
   consistent backup, read-only open, initial enable, format versioning, and
   adoption of existing Git-only repositories without discarding data. Distinguish
   rebuildable caches from authoritative journal files in all cleanup APIs.
2. **Durability and recovery.** Specify versioned, framed, checksummed chunk/root
   or equivalent transaction records, monotonic generations, and stable transaction
   IDs. Write all required data and a commit marker before durable flush and
   acknowledgment. Include file/directory hardening for creation and rotation.
   Recover to the last complete valid committed transaction; distinguish incomplete
   tails from corruption inside acknowledged history. Define bounded replay and
   safe compaction. Git export must never be required to recover an acknowledged
   SQL write.
3. **Concurrency and outcomes.** Retain pinned readers, process-safe coordination,
   and stale-writer detection against the working generation. Specify how other
   processes discover journal progress; the unchanged Git head is insufficient.
   Embedded and MySQL paths use the same persistence logic. Preserve rejected,
   committed, and unknown outcomes: SQL recovery uses journal transaction IDs,
   checkpoint recovery uses Git candidate IDs. Define compatible recovery APIs and
   avoid reporting failures after durable journal commit as definite rollback.
4. **Checkpoint protocol.** Capture one durable generation, materialize and harden
   its reachable Git graph, then conditionally publish the data ref. Durably record
   the checkpoint association before retiring journal data. Define interruption
   recovery between ref publication and journal bookkeeping, with stable checkpoint
   identity/generation metadata so retries do not duplicate successful commits.
   Choose writer serialization or a pinned checkpoint with concurrent later writes;
   later generations must remain dirty. Initial serialization is acceptable if its
   pause is measured. Protect active readers and unpublished chunks during journal
   compaction and Git GC. Unchanged checkpoints should create no new history.
5. **Sync and hooks.** Initially require clean working state for synchronization
   and reconciliation, returning an actionable error when dirty. Recheck cleanliness
   under coordination before any local transition; tracking fetches alone may
   advance independently. Update the clean journal base and Git head recoverably,
   preserving pinned readers and stale-writer detection. Hooks are opt-in, compose
   with existing hooks/managers, and never provide durability. Start with a
   noninteractive warning/refusal. A combined code/data action remains two separate
   commits: account for code-commit failure after a successful data checkpoint.

**Prototype and benchmarks:** retain the native-Git path as the paired control.
Use identical SQL semantics, fixtures, mutation sequences, and declared durability
assumptions. Keep required flushes enabled; a memory-only journal is not a valid
comparison. Publish raw samples, settings, exact revision, machine/filesystem,
commands, and limitations in `docs/m4.3-bench.md`.

- Repeat exact-key updates, inserts, and deletes at 1k / 10k / 50k rows with varied
  keys and payloads. Assert that every timed mutation changes durable state and
  verify its result. Measure acknowledgment latency, p50/p95 with adequate samples,
  allocations, live/peak memory, bytes appended, flushes, and lock wait/hold.
- Add a realistic fixture: 1,000 issues, 100 authors, 10,000 comments, and 50,000
  events with documented distributions. Measure a 30–50-issue board including
  authors and comment summaries, issue detail, state changes, and comment creation.
  Use actual supported SQL and include required scans/joins rather than assuming
  secondary indexes or range pushdown. Exercise embedded and persistent MySQL
  clients with transaction boundaries included.
- Compare batches of 1/10/100 changes. Separate SQL save, data checkpoint, checkpoint
  writer pause, and full-sync time; also report total save + checkpoint + sync cost.
  Prove SQL saves do not create Git snapshots or advance the data ref, then verify
  a checkpoint reconstructs identical schemas/rows in a fresh clone. Moving cost
  off the save path must remain visible in checkpoint measurements.
- Measure reopen/replay with growing journals, clean checkpoint reopen, and reads
  after another process advances working state. Cover two process writers, linked
  worktrees, and writes overlapping checkpoints/sync. Separate startup/recovery
  from warm-operation timings and account for checkpoint/compaction stalls.
- Evaluate approximately **10 ms for a durable small save** and **50 ms for a warm
  board request** as product targets on the declared reference environment, not
  existing guarantees. Include full acknowledgment/result delivery. Do not use
  an in-transaction point lookup to represent an entire board request. Report
  missed targets, bottlenecks, and separate cold-start/checkpoint expectations.

**Correctness and exit:** publish the design, executable prototype, paired results,
and an adopt/revise decision with migration/rollout work assigned. Demonstrate
recovery of acknowledged SQL writes after process termination without a data
commit; rollback and failed statements expose no partial edits. Inject failures
around append, flush, ref publication, checkpoint bookkeeping, journal rotation,
and compaction. Verify outcome recovery, dirty-sync refusal, cross-process
conflicts, and checkpoint reconstruction after cache deletion/GC. Run full and
race tests plus build/diff checks; process-kill tests do not establish power-loss
behavior on every filesystem. Prototype completion does not imply target latency
or production readiness. Update storage, SQL, sync, recovery, and user docs before
switching defaults; M5 takes the selected rollout and application work.

### M5 — Complete the accepted integration and prove the tool-author experience

- Complete adoption/migration work selected by M4.3, then build examples around
  durable SQL saves, intentional data commits, and explicit sync.
- Connect M4 reconciliation to the Git integration established in M0/M3, with no
  long-running RepoDB service required just to transport refs.
- Build a small embedded issue-tracker example and a second namespace for agent
  events, plus a MySQL client example against the same engine.
- Validate worktree switching, concurrent embedded/server use, read-only clones,
  missing executables, hook failures, network failures, and partial synchronization.
- Document enable/embedded/start, status/diff/data-commit, journal backup/recovery,
  sync, and any optional hook policy.
  Keep explicit `repodb sync` visible in examples; ordinary source pushes do not
  publish database changes under the accepted contract.

**Exit:** two users clone and enable a project, use different SQL-backed tools
offline, checkpoint data directly or through tooling, then synchronize and see
both users' committed changes. Uncheckpointed work survives restart. Neither
tool implements journal persistence, Git storage, or merging.

### M6 — Broaden compatibility and scale from measured workloads

- Expand types, collations, schema migrations, constraints, secondary indexes,
  and MySQL driver/ORM compatibility with behavioral tests.
- Measure append-heavy traces and mutable issue/board data: commit latency,
  query latency, memory use, repository growth, and synchronization cost.
- Optimize tree mutation, Git object access, and caches from those measurements.
  Carry forward the ranked M4.2 backlog for incremental mutation, subtree diff,
  memory/I/O improvements, or format/backend changes, with measured justification.
- Add fault injection, corruption handling, fuzzing, retention/compaction policy,
  and a supported authentication/TLS model for standalone network use.

**Exit:** publish compatibility and workload limits backed by repeatable tests.
Full MySQL parity and unrestricted OLTP performance are not initial release claims.

## Immediate next deliverable

Deliver M4.3's working-state design and paired journal/native-Git prototype
benchmark. Separate durable SQL saves from intentional data commits; evaluate
the 10 ms save / 50 ms warm board targets on a realistic fixture. Record the
adoption and migration decision before M5. Deferred M4.2 characterization remains
explicitly assigned to M5/M6 rather than blocking this focused experiment.

## References

- [Go profiling and tracing](https://go.dev/doc/diagnostics)
- [Git Trace2 performance instrumentation](https://git-scm.com/docs/api-trace2)
- [Dolt journal persistence design](https://www.dolthub.com/blog/2023-01-04-acid-transactions/)
- [Dolt SQL commits versus history commits](https://www.dolthub.com/docs/other/faq/#whats-the-difference-between-commit-and-dolt_commit)
- [Git clone behavior](https://git-scm.com/docs/git-clone)
- [Git push and refspec selection](https://git-scm.com/docs/git-push)
- [Git hooks and their execution points](https://git-scm.com/docs/githooks)
- [Conditional ref updates](https://git-scm.com/docs/git-update-ref)
- [Remote fetch/push configuration](https://git-scm.com/docs/git-config)
- [Dolt's Git remote implementation direction](https://www.dolthub.com/blog/2026-02-13-announcing-git-remote-support-in-dolt/)
