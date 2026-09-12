# RepoDB implementation plan

Status: M0 through M4.1 complete; M5 is next. Updated 2026-09-12 after the
[M4.1 implementation and measurements](m4-bench.md). Later milestones remain
pending.

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
- Keep authoritative schema and data in Git objects reachable from dedicated
  RepoDB refs. Local caches and indexes may be rebuilt from repository state.
- Persist successful SQL transactions automatically. Users need not stage files,
  export tables, or manually create Git snapshots for ordinary persistence.
- Keep database changes out of the source worktree, index, and code branch history.
- Support multiple applications and independent clones, including offline writes
  and synchronization without silent data loss.
- Make repository integration an explicit, idempotent `repodb enable` operation.
  A normal clone gets the code; enable discovers/fetches database refs and sets up
  their future transport. Enable does not start a server.
- Offer `repodb start` for users who want a standalone MySQL server. Embedded
  applications own the lifecycle of their engine instances.

User-facing workflow:

```sh
# Regular git clone
git clone <project-url>
cd <project>

# Fetch database refs and configure future fetches; no hooks or service required
repodb enable --remote origin

# Optional: start the MySQL server; embedded applications open the engine directly
repodb start

# Explicitly synchronize database changes with the selected remote
repodb sync --remote origin
```

Transparency means persistence and Git storage are handled by RepoDB. Genuine
concurrent editing conflicts still need an application policy or user resolution.
Local SQL success does not imply that an offline or failing remote has received it.

M0 established an explicit transport contract: enable adds a fetch refspec into
`refs/repodb/remotes/<remote>/data`, preserves existing configuration, and installs
neither push refspecs nor hooks. Ordinary `git push` does not publish database
changes; `repodb sync` is the guaranteed database network operation. Engine open
and new transaction boundaries inspect already fetched state without implicit
network I/O. See [the accepted command matrix](git-integration.md).

## What exists today

| Component | Current implementation | Required change |
| --- | --- | --- |
| `server/` | MySQL listener routed through the persistent shared engine | Broaden compatibility after merge correctness |
| `client/` | MySQL driver wrapper | Retain as optional network client |
| `common/prolly/` | Deterministic bulk tree build, point lookup, iteration, and graph validation | Add streaming sorted iteration/building in M4.1; incremental mutation later |
| `common/storage/` | SHA-256-addressed store interface and memory/filesystem implementations | Retain interface; repository snapshots and writers provide Git-backed stores |
| `common/repository/` | Versioned snapshots, outcome recovery, graph validation, shared publication lock/CAS, two-parent merge candidates, and legacy import | Reuse validated snapshots and existing roots while preserving publication guarantees |
| `common/git/` | Object/tree/commit operations, classified expected-head updates, fsync, explicit remote transport, merge-base lookup, and exact-candidate pushes | Remove duplicate reads; profile validation caching and batched writes in M4.1 |
| `engine/` and `integration/` | Persistent SQL, three-way merging, explicit sync, and durable conflicts | Reduce whole-table materialization and make unchanged sync cheap |
| CLI | `init`, `status`, `import-legacy`, `enable`, `sync`, `conflicts`, `resolve`, `start`, and network `sql` | Broaden compatibility after merge correctness |

Existing tests cover tree determinism and validation, persistent embedded and
MySQL SQL, restart recovery, publication outcomes through both interfaces,
separate-process and linked-worktree contention, injected failures, Git
SHA-1/SHA-256 formats, legacy import, enable/sync idempotence, fast-forward
transport, pinned transactions, disjoint offline merging, durable row/schema
conflicts, explicit resolution, and repeat-sync idempotence. `make test` passes.
Broader compatibility and incremental tree mutation remain pending.

The later M4 benchmark passed engine and integration tests, but its restricted
environment could not bind a loopback socket for the full MySQL wire suite. Its
results establish merge costs, not an additional full-suite validation or a
larger supported workload. M4.1 must run wire tests in a suitable environment.

The original tracked-directory/manual-snapshot design is superseded by this plan.
Its reusable storage and SQL pieces are a starting point, not the target contract.

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

Accepted baseline for M2: each successful write transaction creates one
internal data commit and atomically advances the data ref using its expected old
value. Autocommit statements follow the same path. Explicit rollback publishes
nothing. Unchanged transactions need not create new commits. This deliberately
replaces the prototype's rule against automatic Git commits; these commits live
in independent database history.

Persist objects before publishing the ref and acknowledge success only after the
required durability steps. An interrupted write leaves either the previous state
or a complete new state. Record filesystem/fsync and Git-version assumptions;
atomic ref replacement alone is not a complete power-loss durability guarantee.
Benchmark this baseline before introducing batching or a write-ahead log, which
would change the relationship between acknowledged writes and Git-owned state.

M2 distinguishes definite rejection, successful publication, and unknown commit
outcomes, including recovery through embedded and MySQL interfaces. Preserve this
contract during optimization: a post-publication verification error must not
imply rollback or authorize automatic replay.

M2 derives inventories from reachable catalog objects, batches Git reads, and
reuses existing Git object IDs. Historical commits retain older snapshots.
The remaining M4 costs are whole-table copying/rebuilding, materialized three-way
row merges, repeated eager snapshot validation, and per-object Git writes.
M4.1 addresses these measured costs without changing the data format, merge
policy, explicit sync contract, or acknowledged-write durability model.

### Local concurrency and remote concurrency

Discover the common Git directory, including linked worktrees, instead of assuming
`.git` is a directory. Repository-wide state and locking belong to that shared
identity. Readers pin immutable snapshots; publication is serialized across
processes and checked against the expected data head. A stale writer must receive
a defined retryable conflict or pass explicit validation, never overwrite a newer
head. Do not transparently replay arbitrary application transactions.

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

### M5 — Complete the accepted integration and prove the tool-author experience

- Build on the M4.1 measurements and documented workload bounds.
- Connect M4 reconciliation to the Git integration established in M0/M3, with no
  long-running RepoDB service required just to transport refs.
- Build a small embedded issue-tracker example and a second namespace for agent
  events, plus a MySQL client example against the same engine.
- Validate worktree switching, concurrent embedded/server use, read-only clones,
  missing executables, hook failures, network failures, and partial synchronization.
- Document the enable/embedded/start workflows and supported Git command matrix.
  Keep explicit `repodb sync` visible in examples; ordinary source pushes do not
  publish database changes under the accepted contract.

**Exit:** two users clone and enable a project, use different SQL-backed tools
offline, then synchronize and see both users' changes. Neither tool implements
Git object storage or merging; neither user manages database snapshots.

### M6 — Broaden compatibility and scale from measured workloads

- Expand types, collations, schema migrations, constraints, secondary indexes,
  and MySQL driver/ORM compatibility with behavioral tests.
- Measure append-heavy traces and mutable issue/board data: commit latency,
  query latency, memory use, repository growth, and synchronization cost.
- Optimize tree mutation, Git object access, and caches from those measurements.
  Carry forward only explicitly documented M4.1 deferrals for validation caching
  and batched object writes; do not defer root reuse or streaming merge implicitly.
- Add fault injection, corruption handling, fuzzing, retention/compaction policy,
  and a supported authentication/TLS model for standalone network use.

**Exit:** publish compatibility and workload limits backed by repeatable tests.
Full MySQL parity and unrestricted OLTP performance are not initial release claims.

## Immediate next deliverable

Proceed to M5 on the optimized merge foundation. Build the tool-author examples
and complete integration coverage for worktrees, concurrent embedded/server use,
read-only and partial synchronization failures, while retaining explicit sync as
the visible reconciliation boundary and the measured small-database scope.

## References

- [Git clone behavior](https://git-scm.com/docs/git-clone)
- [Git push and refspec selection](https://git-scm.com/docs/git-push)
- [Git hooks and their execution points](https://git-scm.com/docs/githooks)
- [Conditional ref updates](https://git-scm.com/docs/git-update-ref)
- [Remote fetch/push configuration](https://git-scm.com/docs/git-config)
- [Dolt's Git remote implementation direction](https://www.dolthub.com/blog/2026-02-13-announcing-git-remote-support-in-dolt/)
