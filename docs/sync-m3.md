# M3 enable and synchronization contract

M3 transports one RepoDB data history with explicit commands. It supports
initial transfer and fast-forward synchronization. M4 adds reconciliation on
top of this transport contract.

## Enable

Select a remote explicitly:

```sh
repodb enable --remote origin
```

Enable verifies the named Git remote and idempotently adds:

```text
+refs/repodb/data:refs/repodb/remotes/origin/data
```

It preserves every existing fetch refspec and writes `repodb.remote=origin` as
diagnostic configuration. It adds no push refspec, installs no hook, and starts
no process. Repeating the command does not duplicate configuration.

If the remote contains RepoDB data and the clone has no local data history,
enable fetches, fully validates, and adopts that history. If neither side has a
data history, enable initializes an empty local catalog. If local data already
exists, fetched data remains in the tracking ref until explicit sync.

## Sync

```sh
repodb sync --remote origin
```

Sync performs these steps:

1. Fetch the remote data ref into its separate tracking ref.
2. Validate the manifest, inventory, schema, complete Prolly graph, row values,
   and primary-key encodings.
3. Acquire the same common-Git-directory publication lock used by SQL commits.
4. Re-read the local data head and compare ancestry.
5. Fast-forward the local ref or perform a normal fast-forward Git push.
6. Refresh the tracking ref after a push.

The lock and expected-head checks prevent sync from overwriting a concurrent SQL
commit. A SQL transaction already in progress keeps its immutable snapshot. If
sync advances the local head, that transaction can continue reading its pinned
state; its later commit receives the normal stale-writer conflict.

Sync never force-pushes. M4 now reconciles divergence with a common-ancestor
three-way merge and bounded retry behavior; see [merge-m4.md](merge-m4.md).

Local SQL durability and remote transport remain separate outcomes. A successful
SQL commit does not imply successful sync, and a fetch or push failure does not
damage the local committed database.
