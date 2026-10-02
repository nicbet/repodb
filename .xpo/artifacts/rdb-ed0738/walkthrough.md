# rdb-ed0738 walkthrough: a failed COMMIT ends the transaction

Fixed on the rdb-e0c717 branch (see its walkthrough for context).

**Bug.** go-mysql-server clears the session's transaction (`ctx.SetTransaction(nil)`, and `SetIgnoreAutoCommit(false)` for explicit transactions) only after `CommitTransaction` succeeds. RepoDB's transaction can't be committed twice: its snapshot writer is marked committed on the first attempt. After a rejected commit, the session therefore kept a dead transaction:
- reads showed its unpublished rows;
- the next write failed with a duplicate key or `snapshot writer is already committed`.

The documented advice "retry the whole transaction" didn't work.

**Fix.** `session.CommitTransaction` (`engine/catalog.go`) clears the transaction and explicit-transaction mode whenever the native-git or journal commit returns an error. This matches MySQL, where a failed commit rolls the transaction back and the session returns to autocommit. The next statement starts a fresh transaction on the current snapshot.

**Test.** `TestRejectedCommitStartsFreshTransaction`, native-git and journal:
1. `START TRANSACTION`, insert, a concurrent commit elsewhere, then `COMMIT` → `ErrConflict`;
2. the session sees only published rows;
3. the retried transaction commits.

It fails without the fix.

**Docs.** `docs/sql.md` states that a failed `COMMIT` ends the transaction.
