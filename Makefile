.PHONY: build test fmt m0 m2-bench

build:
	go build -o bin/repodb ./cmd/repodb
	go build -o bin/repodb-server ./cmd/repodb-server

test:
	go test ./...

fmt:
	gofmt -w client cmd common server

m0:
	go run ./experiments/gitstorage -root /tmp/repodb-m0

m2-bench:
	go run ./experiments/m2bench -root /tmp/repodb-m2
