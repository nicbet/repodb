# Durable working state prototype

Status: M4.3 opt-in prototype, 2026-09-13. The native-Git transaction path
remains the default while the benchmark and failure matrix are completed.

## State and authority

RepoDB now models three distinct versions:

1. `refs/repodb/data` names the last intentional, shareable data commit.
2. The working journal names a durable local generation layered on that commit.
3. A SQL transaction pins one working generation and keeps its edits private
   until SQL `COMMIT`.

The prototype is selected with `engine.Options{Persistence:
engine.PersistenceJournal}` or `repodb start --persistence journal`. Native-Git
persistence remains the default and the paired benchmark control.

Authoritative journal data is stored at
`<git-common-dir>/repodb/working/v1/journal`. It is shared by linked worktrees,
never enters the source worktree or index, and must not be removed by cache
cleanup. `<git-common-dir>/repodb/cache/` remains disposable. A repository backup
that promises recovery of uncommitted SQL work must copy the Git repository and
the journal at one coordinated point; copying only Git reconstructs the last data
commit. Read-only tooling may open the committed Git snapshot without the journal,
but must describe that view as committed rather than current working state.

Existing Git-only repositories adopt the prototype without migration: an absent
journal means generation zero, clean, based on the current data head. A future
format cannot silently reinterpret `v1`; unsupported versions fail open.

## Record and durability protocol

The append-only file contains independently framed records:

```text
"RDBJ" | payload length (u32 BE) | CRC-32C (u32 BE) | JSON payload
```

Every changing SQL transaction gets a random 128-bit transaction ID and the next
monotonic generation. A `prepare` frame carries the complete typed manifest and
new content-addressed Prolly/schema blobs. A following `commit` frame repeats the
transaction ID, generation, and committed base. RepoDB appends both frames and
calls `fsync` before acknowledging SQL success. Recovery applies only matched,
ordered prepare/commit pairs. A short final frame is an incomplete tail and is
ignored; invalid framing, checksums, generation order, or markers within complete
history are corruption and fail open.

The journal, not Git export, is sufficient to recover acknowledged SQL writes.
The prototype currently replays the complete file and retains reachable object
bytes in memory. Bounded replay, rotation, directory hardening on first creation,
and online compaction are rollout work, not properties of this prototype.
Compaction must write and fsync a replacement, fsync its directory rename, and
retain all data reachable by pinned readers before retiring an old segment.

An append or flush error before acknowledgment has an unknown outcome unless
`WorkingState.RecoverTransaction` proves whether its stable transaction-ID marker
is durable. An injected error after flush deliberately returns a typed committed
error while retaining the durable snapshot, preventing callers from treating
every error as rollback. SQL exposure of that recovery API is still pending.

## Concurrency

Transactions pin immutable snapshots. Journal publication takes a process-safe
`working.lock`, reloads the journal, and compares the writer's expected generation
and committed base. A stale writer receives `repository.ErrConflict`; RepoDB does
not replay application SQL. Each transaction boundary reloads journal progress,
so another process can advance working state without changing the Git ref. The
embedded and MySQL paths both call the same engine transaction implementation.

Linked worktrees share the lock and journal. The initial checkpoint implementation
holds `working.lock` while exporting to Git, serializing writers and making the
pause directly measurable. The sync implementation refuses a journal it observes
as dirty. Holding one coordination lock across every local sync transition remains
required before default adoption; the current pre-transition recheck narrows but
does not eliminate that race.

## Intentional checkpoint

`repodb commit -m "message"` performs the prototype checkpoint:

1. lock and pin the latest durable working generation;
2. copy only objects absent from the committed base into a repository writer;
3. materialize and harden the complete reachable Git tree;
4. conditionally advance `refs/repodb/data` from the recorded base;
5. append and fsync a checkpoint association `(generation, old base, Git commit)`.

An unchanged checkpoint creates no commit. After a successful checkpoint, the
same generation is clean and later SQL saves advance from it. If the process dies
after ref publication but before journal bookkeeping, open compares the published
manifest with the durable working manifest and recognizes an identical snapshot
as clean. This avoids duplicate history, but the prototype does not yet persist a
candidate Git ID before ref publication; exact checkpoint-outcome recovery and
journal rotation fault injection remain rollout requirements.

Git GC is safe for committed data because the checkpoint tree supplies real Git
reachability edges. Unpublished objects exist only in the authoritative journal
and must be protected by journal-aware cleanup and backup procedures.

## Status, diff, sync, and hooks

`repodb status` reports the committed head plus journal generation/dirty state.
`repodb diff` reports added, modified, and deleted table roots; row-level rendering
is not implemented. `repodb sync` returns an actionable error when it observes a
dirty journal, so it never intentionally checkpoints local SQL changes. A fetch of
a tracking ref is non-authoritative and may occur independently; any local
fast-forward or merge must recheck cleanliness under final coordination before
the prototype can become the default.

Hooks are outside this prototype. A future pre-commit diagnostic may warn or
refuse according to explicit configuration, must compose with existing hook
managers, and cannot participate in SQL durability. A data checkpoint followed by
a failed code commit leaves a valid independent data commit; no atomic code/data
claim is made.

## Decision

**Revise, do not adopt as the default yet.** The prototype is sufficient to
measure the main latency trade: one framed append and flush for SQL save versus
Git object/tree/commit/ref work, with the displaced export cost measured as a
checkpoint. Adoption is gated on the published M4.3 matrix and these corrections:

- durable directory creation, bounded recovery, rotation, and compaction;
- public transaction-ID and checkpoint-candidate outcome recovery;
- one lock-order protocol shared by working saves, checkpoint, sync, and GC;
- row-level diff plus migration/rollback tooling;
- process-kill, fault, linked-worktree, MySQL, and fresh-clone reconstruction tests.

This decision preserves the working-state product contract while avoiding a
premature default change based on an incomplete durability implementation.
