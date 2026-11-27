.PHONY: build test race generate verify demo

build:
	go build -o bin/proxydiag ./cmd/proxydiag

test:
	go test ./...

race:
	go test -race ./...

generate:
	go run ./cmd/proxydiag generate -seed 424242 -out samples

verify:
	go run ./cmd/proxydiag verify -samples samples

demo:
	go run ./cmd/proxydiag demo -samples samples
