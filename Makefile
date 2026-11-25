.PHONY: help test race vet offline-build gen demo verify clean

help:
	@echo "make test          run unit/integration tests"
	@echo "make race          run race detector"
	@echo "make demo          generate seed-42 samples and verify them"
	@echo "make verify        verify existing samples/seed-42"
	@echo "make offline-build build with module downloads disabled"

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

gen:
	go run ./cmd/hckernel gen -out samples/seed-42 -seed 42

demo:
	go run ./cmd/hckernel demo -seed 42

verify:
	go run ./cmd/hckernel verify -dir samples/seed-42

offline-build:
	GOPROXY=off go build ./...

clean:
	rm -rf artifacts/hckernel
