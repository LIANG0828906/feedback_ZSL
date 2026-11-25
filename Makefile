.PHONY: all test race vet fmt build verify verify-parallel serve repl

all: fmt vet test build

fmt:
	gofmt -w cmd internal tests

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./internal/engine ./internal/incremental ./internal/checkpoint ./tests

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -o bin/mvccwb ./cmd/mvccwb

verify: build
	./bin/mvccwb verify-all

verify-parallel: build
	./bin/mvccwb verify-all --parallel

serve: build
	./bin/mvccwb serve --addr 127.0.0.1:18080

repl: build
	./bin/mvccwb repl
