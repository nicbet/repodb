.PHONY: build test fmt bench bench-external m0 m2-bench m4.2-bench m4.3-bench
.DEFAULT_GOAL := build

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

test:
	go test ./...

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
