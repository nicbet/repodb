# Spec: Default --remote flag to saved repodb.remote config

## What

When `--remote` is omitted from `sync`, `conflicts`, and `resolve`, fall back to the `repodb.remote` value saved in local Git config by `enable`.

## Why

After running `repodb enable --remote origin`, users must still pass `--remote origin` on every subsequent `sync`, `conflicts`, and `resolve` invocation. This is redundant — `enable` already persists the remote name to Git config, but nothing reads it back.

## How

### 1. New helper: `resolveRemote()`

Add a function in `integration/integration.go` alongside `requireRemote()`:

```go
func resolveRemote(ctx context.Context, start string, explicit string) (cli repodbgit.CLI, info repodbgit.Info, remote string, tracking string, err error)
```

This function:
1. Creates the `CLI{}` and calls `cli.Discover(ctx, start)` to get `info`
2. If `explicit` is non-empty, uses it as `remote`
3. If `explicit` is empty, reads `repodb.remote` from Git config via `cli.ConfigValues(ctx, info.TopLevel, "repodb.remote")`
   - If exactly one value is returned, uses it as `remote`
   - If zero values, returns `ErrRemoteRequired`
   - If multiple values, returns `ErrRemoteRequired` (ambiguous config — don't guess)
4. Calls `TrackingRef(remote)` to get `tracking`
5. Returns all values needed by callers

This bundles the discover + remote-resolution steps that all three callers share, avoiding duplicated config-reading logic.

### 2. Update `Sync()`

Replace lines 112–120:
```go
tracking, err := requireRemote(remote)
// ...
cli := repodbgit.CLI{}
info, err := cli.Discover(ctx, start)
```

With a single call to `resolveRemote(ctx, start, remote)` and use the returned `cli`, `info`, `remote`, `tracking`.

### 3. Update `Conflicts()`

Replace lines 45–52 (the `requireRemote` + `repository.Discover` calls) with `resolveRemote()`. The returned `info.TopLevel` feeds into `repository.Discover`.

### 4. Update `Resolve()`

Same pattern — replace `requireRemote` + `repository.Discover` with `resolveRemote()`.

### 5. `Enable()` unchanged

`Enable` continues to use `requireRemote()` directly — it must have an explicit remote since it's the command that *sets* the config value.

### 6. Error message update

Update `MergeConflictError.Error()` to omit `--remote` from the hint when the remote came from config (it would be redundant). The simplest approach: since `MergeConflictError` already has `Set.Remote`, the message can say `--remote %s` regardless — the user can always pass it explicitly even when it's optional. No change needed here.

### 7. Tests

Add a test in `integration/` that:
- Initializes a Git repo with `repodb.remote=origin` in config
- Calls `Sync` / `Conflicts` / `Resolve` with empty `remote`
- Verifies they resolve to `origin` from config
- Verifies that an explicit `--remote` overrides the config value
- Verifies that missing config + empty flag still returns `ErrRemoteRequired`

## Acceptance Criteria

- [ ] `repodb sync` (no `--remote`) works when `repodb.remote` is set in Git config
- [ ] `repodb conflicts` (no `--remote`) works when `repodb.remote` is set in Git config
- [ ] `repodb resolve` (no `--remote`) works when `repodb.remote` is set in Git config
- [ ] Explicit `--remote` overrides the saved config value
- [ ] Missing config + no `--remote` still returns the existing `ErrRemoteRequired` error
- [ ] `repodb enable` still requires explicit `--remote` (no fallback)
- [ ] Tests cover config fallback, explicit override, and missing-config paths
