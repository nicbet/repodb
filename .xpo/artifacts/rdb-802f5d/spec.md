# dbbench: tolerate files vanishing during size measurement

## What / Why

`directoryBytes` fails the whole fixture when Git's background auto-gc deletes a loose object between `WalkDir` listing it and `lstat`. The native-git sync workloads are then never measured (see the issue).

## How

In `directoryBytes`, treat `fs.ErrNotExist` from the walk callback's `err` or from `d.Info()` as "gone": contribute 0 and continue (return `nil`; for a vanished directory, `filepath.SkipDir` is unnecessary because the walk just gets the error for it). All other errors still fail. A comment explains the concurrent auto-gc.

Also check how a `directoryBytes` error propagates today. If a measurement error aborts the remaining workloads of a fixture, it should instead be recorded on that measurement. That extra part applies only if the code shows it's needed; otherwise the ErrNotExist fix is sufficient.

## Acceptance criteria

- A unit test: `directoryBytes` over a tree where a file is removed mid-walk. Deterministic version: a directory entry removed after listing, simulated by walking a directory whose file is deleted from a WalkDir hook; or by testing an extracted `entryBytes` helper directly with a vanished file.
- A native-git smoke run (`-rows 100 -clients 1,4 -requests 2`) completes all workloads, including the four sync workloads.
- `make lint`, `make test`.
