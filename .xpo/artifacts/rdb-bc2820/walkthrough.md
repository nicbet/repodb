# Walkthrough: Default --remote flag to saved repodb.remote config

## What changed

After `repodb enable --remote origin`, the `sync`, `conflicts`, and `resolve` commands now fall back to the `repodb.remote` value in local Git config when `--remote` is omitted. Previously all three commands required `--remote` on every invocation despite `enable` already persisting the remote name.

## How it works

### `resolveRemote()` helper — `integration/integration.go`

A new function bundles two steps every caller needed: `CLI.Discover()` (to find the Git repo root) and remote resolution. When the explicit remote argument is empty, it reads `repodb.remote` from local Git config via `ConfigValues`. If exactly one value is found, it uses that; if zero or multiple values are present, it returns `ErrRemoteRequired` (ambiguous config is treated the same as missing config — no guessing).

The function returns `(cli, info, remote, tracking, err)` — everything the callers need after resolution — so it replaces both the old `requireRemote()` call and the separate `CLI{}`/`Discover()` boilerplate in each function.

### Callers updated

- **`Sync()`** — replaced `requireRemote()` + `CLI{}` + `Discover()` (3 steps) with a single `resolveRemote()` call. The rest of the function is unchanged.
- **`Conflicts()`** — replaced `requireRemote()` with `resolveRemote()`. Now uses `info.TopLevel` from the resolver for `repository.Discover()` instead of passing `start` directly — this is more correct since `start` could be a subdirectory.
- **`Resolve()`** — same pattern as `Conflicts()`.

### Unchanged

- **`Enable()`** keeps using `requireRemote()` directly — it must always receive an explicit `--remote` since it's the command that writes the config value.
- **`requireRemote()`** is retained since `Enable()` still uses it.
- **CLI (`main.go`)** — no changes needed. The `--remote` flag defaults to `""`, which naturally flows into `resolveRemote()` and triggers the config fallback.

## Key decisions

1. **Ambiguous config (multiple `repodb.remote` values) → error, not heuristic.** `ConfigValues` returns all values for a key. If someone manually added multiple entries, picking one would be surprising. Returning `ErrRemoteRequired` forces them to pass `--remote` explicitly, which is safe and clear.

2. **`resolveRemote` bundles discovery.** Rather than adding a separate "read config" step to each caller, the helper returns the CLI and repo info alongside the resolved remote. This avoids duplicating the discover-then-read-config pattern in three places and ensures the config is always read from the correct repo root.

3. **`Conflicts` and `Resolve` now discover via `info.TopLevel`.** Before this change, they passed `start` (which could be a subdirectory) directly to `repository.Discover`. Now they use the canonical top-level path from Git discovery, which is slightly more robust.

## Tests

Added `TestResolveRemoteFromConfig` with 6 subtests:
- Missing config + no flag → `ErrRemoteRequired`
- Sync with empty remote → resolves from config
- Explicit `--remote` overrides config value
- Conflicts with empty remote → resolves from config
- Resolve with empty remote → creates a real two-clone row conflict, resolves it with `Resolve(ctx, other, "", ...)`, verifying the full Resolve → Sync path through config fallback
- Enable with empty remote → still returns `ErrRemoteRequired`
