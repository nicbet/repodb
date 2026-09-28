.PHONY: build test lint cross-windows fmt bench bench-external m0 m2-bench m4.2-bench m4.3-bench
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

m0:
	go run ./experiments/gitstorage -root /tmp/repodb-m0

m2-bench:
	go run ./experiments/m2bench -root /tmp/repodb-m2

m4.2-bench:
	go test ./engine -run '^$$' -bench '^BenchmarkSQL' -benchmem -benchtime=500ms -count=5
	go test ./integration -run '^$$' -bench '^(BenchmarkSyncUpToDateWarm|BenchmarkSyncDivergent)$$' -benchmem -benchtime=1x -count=1

m4.3-bench:
	go test ./engine -run '^$$' -bench '^BenchmarkM43(DurableSave|JournalReplayGrowth)$$' -benchmem -benchtime=30x -count=5
	go test ./engine -run '^$$' -bench '^BenchmarkM43JournalFirstSaveAfterCheckpoint$$' -benchmem -benchtime=10x -count=5
	go test ./engine -run '^$$' -bench '^BenchmarkM43JournalCheckpoint$$' -benchmem -benchtime=1x -count=5
	go test ./engine -run '^$$' -bench '^BenchmarkM43TypedEdit(Journal|Checkpoint)$$' -benchmem -benchtime=30x -count=5
