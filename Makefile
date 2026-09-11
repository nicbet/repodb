.PHONY: build test fmt

build:
	go build -o bin/repodb ./cmd/repodb
	go build -o bin/repodb-server ./cmd/repodb-server

test:
	go test ./...

fmt:
	gofmt -w client cmd common server

