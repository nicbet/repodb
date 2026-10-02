.PHONY: build test lint cross-windows fmt bench bench-external bench-docker bench-go bench-journal
.DEFAULT_GOAL := build

# go-mysql-server's default regex backend needs cgo and ICU4C; use its pure-Go one.
export GOFLAGS += -tags=gms_pure_go

BENCH_MODE ?= native-git
BENCH_ARGS ?=

bench:
	go run ./experiments/dbbench -mode $(BENCH_MODE) $(BENCH_ARGS)

BENCH_DSN ?= root@tcp(127.0.0.1:3306)/
bench-external:
	go run ./experiments/dbbench -mode external -dsn '$(BENCH_DSN)' $(BENCH_ARGS)

# The published scorecard: the harness runs in a Linux container on the same
# Docker VM as the MySQL and Dolt baselines, with RepoDB's fixtures on a named
# volume and baselines reached by container name on the repodb-bench network.
# Measures committed code only: refuses a dirty tree unless BENCH_ALLOW_DIRTY=1.
# See docs/benchmark.md.
BENCH_OUT ?= $(CURDIR)/bench-out
BENCH_IMAGE ?= repodb-dbbench
BENCH_NETWORK ?= repodb-bench
BENCH_VOLUME ?= repodb-bench-fixtures
bench-docker:
	@if [ -n "$$(git status --porcelain)" ] && [ "$(BENCH_ALLOW_DIRTY)" != "1" ]; then \
		echo "bench-docker: uncommitted changes would be measured under commit $$(git rev-parse --short HEAD)."; \
		echo "Commit them, or set BENCH_ALLOW_DIRTY=1 for a diagnostic run (its report records the dirty status)."; \
		exit 1; \
	fi
	docker build -q -f experiments/dbbench/Dockerfile -t $(BENCH_IMAGE) .
	docker network inspect $(BENCH_NETWORK) >/dev/null 2>&1 || docker network create $(BENCH_NETWORK) >/dev/null
	docker volume create $(BENCH_VOLUME) >/dev/null
	docker run --rm -u root -v $(BENCH_VOLUME):/fixtures --entrypoint sh $(BENCH_IMAGE) -c 'rm -rf /fixtures/* && chown bench:bench /fixtures'
	mkdir -p $(BENCH_OUT)
	docker run --rm --network $(BENCH_NETWORK) -v $(BENCH_VOLUME):/fixtures -v $(BENCH_OUT):/out $(BENCH_IMAGE) \
		-mode $(BENCH_MODE) $(if $(filter external,$(BENCH_MODE)),-dsn '$(BENCH_DSN)') \
		-temp-dir /fixtures -output /out/scorecard-$(BENCH_MODE)$(BENCH_SUFFIX).json \
		-revision "$$(git rev-parse HEAD)" -working-tree-status "$$(git status --porcelain)" -runtime docker \
		$(BENCH_ARGS)

build:
	go build -o bin/repodb ./cmd/repodb
	go build -o bin/repodb-server ./cmd/repodb-server

test: cross-windows
	go test ./...

cross-windows:
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=windows GOARCH=amd64 go vet ./...

STATICCHECK_VERSION := v0.8.1

lint:
	@test -z "$$(gofmt -l client cmd common engine experiments integration server)" || { gofmt -l client cmd common engine experiments integration server; exit 1; }
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	GOBIN=$(CURDIR)/bin go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	bin/staticcheck ./...
	GOOS=windows GOARCH=amd64 bin/staticcheck ./...

fmt:
	gofmt -w client cmd common engine experiments integration server

# Go micro-benchmarks: SQL reads/writes per persistence mode, and sync.
bench-go:
	go test ./engine -run '^$$' -bench '^BenchmarkSQL' -benchmem -benchtime=500ms -count=5
	go test ./integration -run '^$$' -bench '^(BenchmarkSyncUpToDateWarm|BenchmarkSyncDivergent)$$' -benchmem -benchtime=1x -count=1

# Durable saves, journal replay growth and checkpoints.
bench-journal:
	go test ./engine -run '^$$' -bench '^Benchmark(DurableSave|JournalReplayGrowth)$$' -benchmem -benchtime=30x -count=5
	go test ./engine -run '^$$' -bench '^BenchmarkJournalFirstSaveAfterCheckpoint$$' -benchmem -benchtime=10x -count=5
	go test ./engine -run '^$$' -bench '^BenchmarkJournalCheckpoint$$' -benchmem -benchtime=1x -count=5
	go test ./engine -run '^$$' -bench '^BenchmarkJournalTypedEdit(Checkpoint)?$$' -benchmem -benchtime=30x -count=5
