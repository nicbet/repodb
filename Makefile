.PHONY: build test fmt m0 m2-bench m4.2-bench

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
