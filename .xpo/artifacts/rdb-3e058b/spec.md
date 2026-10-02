## What
Replace the false comparative sentence in README.md's Performance section.

## Why
Dolt versions data like Git (commits, branches, merges) and syncs across clones through remotes, including plain Git remotes since v1.81.10 (Feb 2026). The README should not make comparative claims we cannot back up.

## How
Replace:

> Neither MySQL nor Dolt provides Git-native version history or cross-clone synchronization.

with a statement about what the benchmark measures, not about what other products can do:

> Dolt also versions, clones, pushes and merges its data, including to Git remotes, but the harness runs sync and merge workloads only against RepoDB, so the sync results have no baseline.

## Acceptance criteria
- README no longer says Dolt lacks version history or sync.
- No other page in README.md or docs/ makes that claim (checked by grep: the only other mention, docs/benchmarks/latest.md "MySQL and Dolt skip RepoDB's Git-specific workloads", is accurate about the harness and stays).