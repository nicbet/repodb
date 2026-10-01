# Docs describe current state only

## What

Restructure `docs/` so that every document describes how RepoDB works **today**. The implementation plan, milestone contracts, prototype write-ups and dated results go. The backlog is the roadmap. `docs/benchmarks/latest.md` is the one performance record.

## Why

`plan.md` (814 lines), `sql-m2.md`, `sync-m3.md`, `merge-m4.md`, `git-integration.md` ("M0 decision"), `working-state.md` ("prototype") and `benchmarks/history/` describe past designs, superseded metrics and old plans. Agents working on RepoDB, and people referencing it, read them as current and draw wrong conclusions.

## Target layout

| File | Content | Sources (verified against code, not copied) |
|---|---|---|
| `docs/cli.md` | Install, clone/enable, server, persistence modes, data lifecycle (status, diff, checkpoint), sync, conflict resolution, backup/recovery, Git interaction, full command reference | `guide.md`, `sync-m3.md`, `merge-m4.md`; `cmd/repodb`, `cmd/repodb-server` |
| `docs/library.md` | Go library: opening an engine, options (persistence modes), sessions, queries and exec, transactions, errors (`ErrConflict` etc.), sync/merge APIs, the MySQL server package, the client package | `guide.md` § Embedded Go API, `sql-m2.md` § Embedded API; `go doc` of `engine`, `server`, `client`, `common/repository` |
| `docs/architecture.md` | Components and how they fit together; the storage format (refs, manifest, object inventory, Prolly trees, key and row encoding, format versions); the persistence modes (native-git snapshot publication, journal working state and checkpoints); transactions, isolation and concurrency (repository-wide optimistic conflicts); sync and three-way merge (tracking refs, conflict records, row identity); durability and recovery; Git integration (ref layout, hooks decision) | `storage-format.md`, `working-state.md`, `sync-m3.md`, `merge-m4.md`, `git-integration.md`, `plan.md` design sections; the code |
| `docs/sql.md` | The supported SQL surface: types, statements, indexes, constraints, collations, transactions and isolation, EXPLAIN; unsupported features, each citing the backlog issue that plans it where one exists | `sql-m2.md`, `guide.md`; `engine` (e.g. `decodeType`, `validateSchema`), tests |
| `docs/testing.md` | The test surface and coverage (user request): each suite (unit, cross-engine, integration, crash/recovery, fuzz, benchmarks), which behaviors it protects and where it lives; the fuzz targets and their properties; known gaps (for example, the suite has not run on Windows; behavior with no tests); how to run everything (tests, race, lint, Windows cross-build, fuzzing, `go test -cover`). No dated results; coverage percentages are produced by the command, not pinned in the doc. | `fuzz-testing.md`, `Makefile`, `*_test.go` |
| `docs/benchmark.md` | Methodology, kept. The publishing process changes: replace latest.md in place, with no history/ archive (git keeps old versions) | — |
| `docs/benchmarks/latest.md` + `latest/*.json` | Kept, unchanged | — |

**Removed:** `plan.md`, `guide.md`, `sql-m2.md`, `storage-format.md`, `working-state.md`, `sync-m3.md`, `merge-m4.md`, `git-integration.md`, `fuzz-testing.md`, and `benchmarks/history/` (all files).

## Writing rules

- **Present tense, current behavior only.** No milestone names (M0–M6), no "prototype", "initial", "decision" or "was". No dated measurements outside `benchmarks/latest.md`; docs may link to it for numbers.
- **Every command, flag, API name, type, constant and error message is checked against the code at the implementing commit.** Where an old doc disagrees with the code, the code wins, and the discrepancy is noted in the completion comment. A doc bug that reveals a code bug is filed separately.
- **Limitations are stated plainly where they apply**, for example: write transactions conflict repository-wide; no reverse index scans; no AUTO_INCREMENT or foreign keys.
- **No roadmap prose in docs/, but limitations cite the backlog.** "Not supported" is fine; "planned for M7" is not. Where a doc states a limitation or an unsupported feature that a backlog issue addresses, it cites the issue ID, e.g. "`AUTO_INCREMENT` is not supported (rdb-a2d28e)". This is required throughout `sql.md`'s unsupported list (user request), and applies to the other docs too (e.g. repository-wide write conflicts → rdb-df092b). IDs are checked against the backlog at writing time. The README stays free of IDs.

## README

- **Roadmap** (no issue IDs), with three sections:
  - *Done*: M0–M6 condensed, plus the range/scan and row-storage work.
  - *Now: finish the performance envelope*: reverse index scans; fewer Git subprocesses per sync; workload profiling; concurrent writers (per-key conflicts, group commit, safe retry, MySQL-compatible errors).
  - *Next: a standalone Git-native relational database*: version-controlled data workflows, secure server, application compatibility, operational durability.
  - A closing line says the roadmap is maintained in the project's issue backlog.
- The docs table lists the new files.
- All links repointed; "Contributing" no longer points to `plan.md`.

## Agent guidance

Add a short "Documentation" section to `AGENTS.md` and `CLAUDE.md`:
- docs/ describe current state;
- a change that alters behavior updates the affected doc in the same issue;
- design history belongs in xpo specs and walkthroughs (searchable with `rationale`), not in docs/.

## Roadmap epic

Update rdb-4c3ae3's "Sequencing" to place rdb-df092b in step 1 (finish performance and publish the supported envelope), matching the README.

## Out of scope

- Legacy experiment code and its Makefile targets (`experiments/gitstorage`, `experiments/m2bench`, `make m0`, `m2-bench`, `m4.2-bench`, `m4.3-bench`). A follow-up issue will be filed if they are stale.
- xpo artifacts (`.xpo/artifacts/*`): these are the historical record by design.

## Acceptance criteria

- `docs/` contains only the files in the target layout, plus `banner.png`.
- No file in `docs/`, `README.md`, `AGENTS.md` or `CLAUDE.md` links to a removed doc; all relative links and anchors resolve (checked by script).
- Every issue ID cited in docs/ exists in the backlog and is not DONE or CANCELED.
- `grep` finds no milestone labels (`M[0-9]`) or "prototype" in `docs/` outside benchmark.md's workload names, and no dated performance numbers outside `benchmarks/latest.md`.
- Every CLI command and flag in `cli.md` matches the `repodb` binary's commands and flags, and every Go identifier in `library.md` exists in the exported API (checked by script where possible, otherwise by review).
- The code examples in `library.md` compile, via a throwaway module or an `Example` test.
- `make lint` passes.

## Decisions

- **Benchmark history** is deleted, and publishing replaces latest.md in place (user: "keep latest.md as the running artifact"; git keeps old versions).
- **`testing.md` documents the test surface and coverage**, not just commands (user: "we should definitely document our test surface/coverage").
- **`sql.md` and `testing.md`** exist alongside the CLI, architecture and library docs.
- **Legacy experiments** get a BACKLOG follow-up; they are not changed here.
- **Issue citations** for unsupported SQL features (user request), extended to limitations in all docs.

## Implementation notes (added during implementation)

- **Facts were verified against the code, not the old docs.** The SQL surface was checked by running statements against a real engine. That included every "not supported" claim that had only been inferred from which go-mysql-server interfaces RepoDB implements. Library examples are compiled against the module.
- **Bugs and gaps found while verifying were filed and are cited where the docs describe the affected behavior:**
  - rdb-53214c: `diff` misses journal row edits;
  - rdb-ff5432: a write inside a READ ONLY transaction panics;
  - rdb-5f12a3: statements and options accepted but not honored (views, JSON keys, prefix indexes, TIME(n), `--database`, FULLTEXT error text);
  - rdb-e0c717: sync/journal race;
  - rdb-c56142: journal as the server default;
  - rdb-8f75fc (comment): `groupSync` exists but the journal lock is held across the flush.
- **Old-doc errors not carried over:** `engine.Open` with options, `engine.RecoverCommit`, the claim that commits bulk-rebuild tables, `repodb commit` as a native-git "no-op confirmation", a plain clone as a backup, AUTO_INCREMENT advice in the merge guidance, and the prototype and adoption-gate language.
- **Error strings still name milestones** (`RepoDB M2 …`). `sql.md` quotes them verbatim; renaming them is part of rdb-f4dec8.
- **`latest.md`** now refers to earlier results through Git history (`d4b1ac6`) instead of `history/`.
