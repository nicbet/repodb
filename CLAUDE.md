<!-- xpo:begin 1.2.1 sha256:67f1ef9b0ccf -->
## Exponential (xpo)

This project uses `xpo` (Exponential) via the MCP server registered in `.mcp.json`.
Always use the MCP tools — never shell out to the `xpo` CLI.

Issue IDs in this project use the prefix `rdb` (e.g. `rdb-a1b2c3`).

### Hard Rules

1. Every code change must be backed by an xpo issue transitioned to DOING before any file is modified.
2. Never start work on a BACKLOG issue without explicit user approval to transition it.
3. Bugs discovered during implementation may be filed and fixed without approval — file the issue, link it to the current work, and fix it.
4. Before beginning any implementation task, load the `xpo` skill and follow it.
5. If an MCP tool call fails, report the error to the user. Never fall back to the CLI.

### Agent Identity

Set the `assignee` field to yourself when transitioning an issue to DOING. Use the form
`<Agent Name> <agent@<host>.local>` — e.g. `Claude Code <agent@macbook.local>`.

<!-- xpo:end -->

## Documentation

- `docs/` describes RepoDB as it works **now**: `cli.md`, `library.md`, `sql.md`, `architecture.md`, `testing.md`, and `benchmark.md` (methodology). It contains no plans, milestone contracts or dated measurements.
- `docs/benchmarks/latest.md` is the only performance record. Replace it in place when publishing a new scorecard (see `docs/benchmark.md`).
- The roadmap is the xpo backlog. Design history and rationale live in xpo specs and walkthroughs; search them with the `rationale` tool, not `docs/`.
- A change that alters behavior, storage, the CLI, the public Go API or the SQL surface updates the affected `docs/` pages in the same issue. Where a doc names a limitation that a backlog issue addresses, cite the issue ID. Verify commands, flags and identifiers against the code rather than copying older text.
