# Latest benchmark results

This file is the one canonical source for RepoDB's current performance. The
methodology and the process for replacing this file are in
[benchmark.md](../benchmark.md). Past reports are in [history/](history/).

## Status: awaiting a fresh run

No scorecard has been measured yet at a single clean commit of the current code.
The most recent full comparison is the
[2026-09-13 M4.4 scorecard](history/2026-09-13-m4.4-scorecard.md). It predates:

- the go-mysql-server upgrade to its latest `main` (Go 1.26, `gms_pure_go` regex
  backend, DECIMAL values as `apd`);
- the fix that stops `repodb commit` from dropping journal rows;
- the fix that keeps secondary indexes correct across journal transactions, and its
  per-generation index-edit cache;
- the fix for readers keeping stale snapshots when another process writes, which
  changed the snapshot fast path that point-read latency depends on.

It was also measured across four different revisions, one with uncommitted
changes. Treat its numbers as indicative of that time only.

A full run of all four modes (native-Git, journal, MySQL 8, Dolt) at one clean
commit will replace this section. When it does, this file will state the revision,
date, machine, operating system, filesystem, Go, Git, and server versions, and the
exact commands, and link its raw JSON in `latest/`.
