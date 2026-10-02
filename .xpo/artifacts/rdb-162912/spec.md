# rdb-162912: Fail fast for stale writers under the publication lock

## What / Why

See the issue. Since rdb-e0c717, native-git publishers wait FIFO on the publication guard. A writer whose base is already stale still writes all its Git objects, tree and commit under the lock before `update-ref` rejects it, so contended native-git throughput collapsed (16 clients: 7.3 → 1.4 ops/s in the probe).

## How (`common/repository/repository.go`, `Writer.CommitWithOutcomeMessage`)

Right after the publication lock is acquired, and before any object is written:
- if the writer has an expected head (`w.expected != ""`), read the current data head with `Repository.Head(ctx)`. Its ref-stamp cache makes an unchanged ref cost a `stat`; a changed ref costs one `rev-parse`.
- if it differs from `w.expected`, return `CommitResult{Outcome: OutcomeRejected}` with `ErrConflict`, exactly what the CAS would have returned, just earlier.

This applies to every publication. Checkpoints (`workingLockHeld`) benefit too, though their head can't move under a dirty journal.

**Correctness.** The CAS in `update-ref` stays as the authority. The early check only rejects writers that the CAS would reject anyway, because the head can't move back while we hold the lock. A failed head read falls through to the existing path, not a false rejection.

## Tests

- `common/repository`: two writers on the same base. After the first commits, the second's commit returns `ErrConflict` with outcome `rejected`, and the repository gains **no** new Git objects (count loose objects or `git count-objects` before and after).
- Existing publication-outcome tests (committed, unknown, rejected; fault points) pass unchanged.
- Contended probe (native-git, 16 sessions × 15 increments, rollback after conflict), recorded in the issue: within ~1.5× of the pre-rdb-e0c717 throughput.

## Out of scope

Lock fairness and conflict rate. The FIFO queue gives the lock-step rejection rate (1 − 1/clients), which is correct for optimistic concurrency. Per-key conflicts and automatic retry are rdb-df092b.
