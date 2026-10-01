# Walkthrough: docs describe current state only

## What changed and why

`docs/` used to mix current behavior with milestone contracts (M0–M4), an 814-line implementation plan, a prototype write-up with "adoption gates", and dated benchmark reports. Agents and readers took all of it as current. Several statements were wrong outright:
- an `engine.Open` signature that doesn't exist, and an `engine.RecoverCommit` function that doesn't exist;
- "each commit bulk-rebuilds the table";
- the claim that a plain `git clone` is a backup;
- AUTO_INCREMENT advice, when AUTO_INCREMENT is unsupported.

The new layout has one document per question a reader or agent asks:

| File | Answers |
| --- | --- |
| `cli.md` | How do I use the `repodb` programs? |
| `library.md` | How do I use RepoDB from Go? |
| `sql.md` | What SQL works, and what doesn't (with backlog IDs for planned expansions)? |
| `architecture.md` | How does it work inside? |
| `testing.md` | What is tested, how do I run it, and what isn't covered? |
| `benchmark.md`, `benchmarks/latest.md` | How is performance measured, and what are today's numbers? |

**Where the rest went:**
- **Roadmap:** the xpo backlog. The README summarizes it without IDs.
- **Design history:** xpo specs and walkthroughs, searchable with `rationale`, and git history.
- **Benchmark history:** git history of `latest.md`. The publishing process now replaces the file in place and compares against the previous commit's JSON, using the unchanged MySQL/Dolt runs as the noise reference.

## How the content was produced

The old docs were used only as a checklist of topics. Every fact was taken from the code at `540737d`:
- **CLI:** commands and flags read from `cmd/repodb` and `cmd/repodb-server`. A script confirms that the commands and flags in `cli.md` match the code.
- **Library:** API names from `go doc`. Every code block in `library.md` is extracted and compiled against the module in a throwaway harness (one full program, 8 fragments).
- **SQL surface:** gathered by a read-only research pass. Every claim it had only inferred from which go-mysql-server interfaces RepoDB implements was then run against a real engine (REPLACE, TRUNCATE, RENAME, foreign keys, views, temporary tables, triggers, procedures, `CREATE DATABASE`, ON DUPLICATE KEY, INSERT IGNORE, INSERT…SELECT, JSON keys, prefix indexes, TIME precision, NOT ENFORCED checks, window functions, CHANGE COLUMN, RENAME INDEX, `information_schema`).
- **Test inventory:** built from every `_test.go` file plus `go test -cover`. Coverage percentages are not pinned in the doc, which says how to produce them.
- **Architecture:** checked against `repository.go`, `working.go`, `prolly/tree.go`, `integration.go` and `engine`. Two first-draft claims were corrected after checking:
  - chunk boundaries come from a rolling hash over entry fingerprints, not a per-entry hash;
  - `publish.lock` is taken after the trees are built in memory, not before.

## Rules that keep it current

- **Current state only.** Present tense. No milestone names, prototype language or dated numbers outside `latest.md`.
- **Cite the backlog.** A limitation that a backlog issue addresses cites the ID, in all docs, not just `sql.md`. For example, repository-wide conflicts cite rdb-df092b. The README stays free of IDs.
- **Agent guidance.** `AGENTS.md` and `CLAUDE.md` gained a "Documentation" section: behavior changes update the docs in the same issue, and identifiers are verified against code. It sits outside the `xpo:begin`/`xpo:end` managed blocks so xpo upgrades don't remove it.
- **Exception: quoted error strings.** `sql.md` quotes error strings verbatim even where they still say "M2". Renaming them belongs to rdb-f4dec8, and `sql.md` must follow.

## Problems found by documenting

Writing down what the code does exposed several problems, all filed:

| Issue | Finding |
| --- | --- |
| rdb-ff5432 (P1) | A write inside `START TRANSACTION READ ONLY` panics in go-mysql-server's read-only validation and kills the process |
| rdb-53214c | `WorkingState.Diff` compares manifest roots, but journal row edits live in a pending overlay, so `repodb diff` misses row-only changes while `status` says dirty |
| rdb-5f12a3 | Accepted but not honored: session-only views, JSON primary keys, ignored prefix lengths, dropped `TIME(n)` precision, a no-op `--database`, a garbled FULLTEXT error |
| rdb-e0c717 | Sync's clean-journal check doesn't hold `working.lock` across the head update, so a concurrent journal commit can leave the working state stuck (`ErrWorkingBaseChanged`) |
| rdb-c56142 | Make journal the server/CLI default; depends on compaction (rdb-515fae) and rdb-e0c717 |
| rdb-8f75fc (comment) | `groupSync` exists, but the journal lock is held across the flush, so each batch is always one commit |
| rdb-f4dec8 (comments) | Legacy experiments and Makefile targets; also dead code (`storage.Filesystem`, `CacheDir`, `common/query`) and milestone-named error strings |

Where these affect documented behavior, the docs state the current behavior and cite the issue. When a fix lands, update the doc in the same issue.

## Roadmap alignment

Step 1 of epic rdb-4c3ae3's "Sequencing" now includes the concurrent-writer epic rdb-df092b and its critical path, matching the README's "Now" section.
