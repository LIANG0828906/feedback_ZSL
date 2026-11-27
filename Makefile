.PHONY: test race verify run

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

verify:
	go run ./cmd/terrain-demo -verify-only -samples samples

run:
	go run ./cmd/terrain-demo -addr 127.0.0.1:8080 -cache .terrain-cache -samples samples
