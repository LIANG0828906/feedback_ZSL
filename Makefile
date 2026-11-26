.PHONY: build test race vet generate verify hard-stop clean

build:
	go build -o bin/strokediag ./cmd/strokediag

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

generate:
	go run ./cmd/strokediag generate -seed 424242 -out artifacts/final-demo

verify:
	go run ./cmd/strokediag verify -seed 424242 -out artifacts/final-acceptance

hard-stop:
	./scripts/hard-stop-demo.sh

clean:
	go clean
	find artifacts -mindepth 1 -maxdepth 1 ! -name workspace.json -exec rm -rf {} +
