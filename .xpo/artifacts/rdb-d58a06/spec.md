# rdb-d58a06: Run the RepoDB scorecard inside Docker

## What / Why

MySQL and Dolt run in Docker Desktop's Linux VM, with data on Docker named volumes (ext4 inside the VM disk), where a synchronous write costs ~0.16 ms. RepoDB ran natively on APFS. Run RepoDB's scorecard in a container on the same VM, with its fixtures on a named volume, so all four systems share the kernel, filesystem, flush semantics and CPU/memory envelope.

**The published scorecard reports only this like-for-like setup** (user decision, 2026-10-02): every system runs in a container on the same Docker VM. Host-native RepoDB numbers are not published. `docs/benchmark.md` describes the setup explicitly.

## How (as built)

### Image (`experiments/dbbench/Dockerfile`, multi-stage)

- **Build stage:** `golang:1.27-trixie`, the same toolchain (Go 1.27.1) as earlier scorecards, which `go.mod`'s `go 1.26.2` minimum allows. `go build -tags gms_pure_go ./experiments/dbbench`. The build context is the repository root; `.dockerignore` excludes `.git`, `.xpo`, `bin`, `bench-out` and `*.test`.
- **Runtime stage:** `debian:trixie-slim` plus `git` and `ca-certificates`. Debian's packaged Git (user decision; 2.47.3 at writing) is recorded in each report. A non-root user `bench` with a global Git identity (fixture repositories make Git commits). Entrypoint `dbbench`, working directory `/fixtures`.

### Clean-tree rule (user decision)

`make bench-docker` refuses to run when `git status --porcelain` is non-empty, naming the commit and the override. `BENCH_ALLOW_DIRTY=1` runs anyway for diagnostics, and the report records the dirty `working_tree_status`.

### Report metadata

New flags `-revision`, `-working-tree-status` and `-runtime` (default `host`). Report construction moved into `newReport(config, root, source)`. An override applies whenever its flag is set, even to the empty string, so a clean status passed from the host is recorded as clean. The report gains `runtime`.

### Make target

`make bench-docker BENCH_MODE=journal|native-git|external [BENCH_DSN=…] [BENCH_SUFFIX=…] [BENCH_OUT=dir] [BENCH_ARGS=…]`:
1. clean-tree check;
2. `docker build -q -f experiments/dbbench/Dockerfile -t repodb-dbbench .`;
3. creates the `repodb-bench` network and the `repodb-bench-fixtures` volume if missing, and empties the volume;
4. `docker run --rm --network repodb-bench -v <volume>:/fixtures -v $(BENCH_OUT):/out repodb-dbbench -mode … [-dsn …] -temp-dir /fixtures -output /out/scorecard-$(BENCH_MODE)$(BENCH_SUFFIX).json -revision … -working-tree-status … -runtime docker $(BENCH_ARGS)`.

There are no CPU or memory limits. `BENCH_OUT` defaults to `./bench-out` (git-ignored).

### Baselines also measured from inside Docker (added during implementation)

The spec's setup said MySQL and Dolt are "reached over loopback TCP between containers", but the old commands ran the harness on the host, through Docker Desktop's port forwarding. That adds latency to every baseline request and isn't like for like either. So `BENCH_MODE=external` runs the same image on the user-defined network `repodb-bench`, reaching `repodb-bench-mysql:3306` and `repodb-bench-dolt:3306` by name. The baseline containers are attached once with `docker network connect`. `BENCH_SUFFIX` (`-mysql`, `-dolt`) keeps their reports apart.

### Durability inside the container

On Linux, `normal` and `full` are both `fdatasync` (rdb-d690aa). The report records `durability: normal`.

### Docs

- `docs/benchmark.md`:
  - a new **Setup** section: where everything runs, storage (named volumes, ext4 in the VM disk), what a flush means there (~0.16 ms, no full flush of the host SSD for any system), default durability per system, no resource limits, one container at a time, recorded metadata;
  - the commands (`make bench-docker` for all four, plus a smoke run);
  - host runs (`make bench`, `make bench-external`) documented as diagnostics, never published;
  - External baselines and Publishing step 1 point to Setup.
- README: the scorecard commands. `testing.md`: the command table and the dbbench tests.

## Tests

- `TestReportRecordsSourceOverrides`: revision, explicitly clean status and runtime are recorded; without overrides they're detected.
- Manual:
  - with a dirty tree, `make bench-docker` refuses (exit 1, message names `BENCH_ALLOW_DIRTY=1`);
  - a diagnostic journal smoke run in Docker (all workloads PASS; report: linux/arm64, go1.27.1, git 2.47.3, host revision, dirty status recorded, `runtime: docker`, `durability: normal`, `git_gc: manual`);
  - an external smoke run against `repodb-bench-mysql:3306` over the network (`server_version` 8.4.11, `runtime: docker`).
- `make test` and `make lint` pass.

## Related bug fixed here: rdb-43dc0e

A 71 MB macOS `dbbench` binary had been committed at the repository root (`e33870e`). It is removed from the tree and `/dbbench` is git-ignored. Its blob stays in history unless the owner chooses a rewrite.

## Decisions (confirmed by the user, 2026-10-02)

1. Publish only the like-for-like Docker setup; no host-native section. `docs/benchmark.md` describes the setup explicitly.
2. Debian's packaged Git in the image, version recorded.
3. `make bench-docker` refuses a dirty tree; `BENCH_ALLOW_DIRTY=1` overrides for diagnostics, with the dirty status recorded.

## Order

Next: a scorecard refresh (new issue) with all four systems in Docker, then the README from those numbers.
