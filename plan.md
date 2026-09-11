# RepoDB implementation plan

Status: M0, M1, and M2 complete; M3 is next. Updated 2026-09-12 after implementation
review and a passing `make test` run. Later milestones remain pending.

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

Proposed user-facing workflow (these commands are not implemented yet):

```sh
# Regular git clone
git clone <project-url>
cd <project>

# Fetch database refs and configure future fetches; no hooks or service required
repodb enable

# Optional: start the MySQL server; embedded applications open the engine directly
repodb start

# Explicitly synchronize database changes with the selected remote
repodb sync
```

Transparency means persistence and Git storage are handled by RepoDB. Genuine
concurrent editing conflicts still need an application policy or user resolution.
Local SQL success does not imply that an offline or failing remote has received it.

M0 established an explicit transport contract: enable adds a fetch refspec into
`refs/repodb/remotes/<remote>/data`, preserves existing configuration, and installs
neither push refspecs nor hooks. Ordinary `git push` does not publish database
changes; `repodb sync` is the guaranteed database network operation. Engine open
and new transaction boundaries inspect already fetched state without implicit
network I/O. See [the accepted command matrix](docs/git-integration.md).

## What exists today

| Component | Current implementation | Required change |
| --- | --- | --- |
| `server/` | MySQL listener and `go-mysql-server` with memory tables | Extract reusable engine; replace memory catalog/storage |
| `client/` | MySQL driver wrapper | Retain as optional network client |
| `common/prolly/` | Deterministic bulk tree build and point lookup | Add iteration, typed SQL encoding, mutation, and diff |
| `common/storage/` | SHA-256-addressed store interface and memory/filesystem implementations | Retain interface; repository snapshots and writers provide Git-backed stores |
| `common/repository/` | Versioned Git snapshots, integrity validation, common-directory discovery, publication lock/CAS, legacy import | Clarify commit outcomes; bound snapshot cost; integrate SQL object-graph validation |
| `common/git/` | Object/tree/commit operations, expected-head ref updates, fsync configuration | Refine publication error classification; add remote transport |
| CLI | `init`, snapshot `status`, `import-legacy`, network `sql`; separate server binary; manual `snapshot` rejected | Introduce enable/start/sync lifecycle and transport status |

Existing tests cover tree determinism/lookup, snapshot reopen and cross-clone
reconstruction, GC/cache deletion, stale and concurrent writers, linked-worktree
identity, Git SHA-1/SHA-256 formats, legacy import, invalid snapshots, the M0 Git
integration experiment, and a basic in-memory SQL round trip over MySQL.
`make test` passes. The writer concurrency test currently uses goroutines in one
process; separate-process contention and publication fault injection remain to
be tested. Persistent SQL, embedded access, SQL transaction semantics, merge
correctness, and the product sync API are not implemented yet.

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

Distinguish definite rejection, successful publication, and an unknown commit
outcome. M1 currently performs a fallible verification read after advancing the
ref, so an error can be returned after publication succeeded. Cancellation or
process failure during ref publication can also leave the caller uncertain.
Define outcome reporting and recovery before exposing SQL commit semantics;
an arbitrary commit error must not imply rollback or authorize automatic replay.

M1 retains every base-snapshot object and rereads the inventory during publication
and validation, with individual Git processes for object access. This is a proof
baseline, not the intended scaling behavior. M2 must define and measure a bounded
initial workload. Build inventories from objects reachable from current catalog
roots when SQL graph traversal exists; historical commits retain older snapshots.
Reuse existing Git object IDs and batch object access as measurements warrant.

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
benchmark are implemented and documented in [docs/sql-m2.md](docs/sql-m2.md).
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

- Implement idempotent `repodb enable` based on M0's proven integration, including
  existing-state discovery and an explicit empty-database initialization path.
- Preserve user hooks/configuration and provide a way to remove only RepoDB-owned
  integration. Install no hooks or push refspecs in the initial enable flow.
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

- Add row/schema diffs, three-way merge, conflict inspection and resolution, and
  validation of supported constraints before advancing the live data ref.
- Cover insert/insert collisions, update/update, update/delete, schema changes,
  and unique-key conflicts between otherwise distinct rows.
- Document distributed row identity: a local auto-increment counter alone cannot
  prevent independently generated IDs from colliding across clones.
- Handle remote advancement during synchronization without force updates or
  unbounded retries. Keep failed merges inspectable and repeatable after restart.

**Exit:** two offline clones add different issues/comments and converge; competing
edits surface as conflicts, survive restart, and converge after explicit resolution.
Repeat synchronization is idempotent and preserves both histories.

### M5 — Complete the accepted integration and prove the tool-author experience

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
- Add fault injection, corruption handling, fuzzing, retention/compaction policy,
  and a supported authentication/TLS model for standalone network use.

**Exit:** publish compatibility and workload limits backed by repeatable tests.
Full MySQL parity and unrestricted OLTP performance are not initial release claims.

## Immediate next deliverable

Proceed to M2 on the existing M0/M1 foundation. First tighten publication outcome
reporting and error classification, with separate-process and injected-failure
tests. Then deliver a small persistent embedded SQL engine shared by the server:
explicit primary keys, a documented small type set, schemas and rows, scans,
autocommit, explicit commit/rollback, and restart recovery through both entry points.

Keep full MySQL compatibility, incremental tree optimization, and distributed merge
outside this first SQL slice. Establish workload bounds while implementing it;
do not extend the in-memory demo while leaving persistence unresolved.

The single local data head, dedicated ref layout, independent data history, and
explicit sync contract are the accepted M0/M1 baseline. M2 applies automatic data
commits to SQL writes. Embedded API signatures and detailed SQL semantics remain
to be specified in M2; merge policy is developed and validated in M4.

## References

- [Git clone behavior](https://git-scm.com/docs/git-clone)
- [Git push and refspec selection](https://git-scm.com/docs/git-push)
- [Git hooks and their execution points](https://git-scm.com/docs/githooks)
- [Conditional ref updates](https://git-scm.com/docs/git-update-ref)
- [Remote fetch/push configuration](https://git-scm.com/docs/git-config)
- [Dolt's Git remote implementation direction](https://www.dolthub.com/blog/2026-02-13-announcing-git-remote-support-in-dolt/)
