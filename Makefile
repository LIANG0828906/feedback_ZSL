.PHONY: test demo generate vet

test:
	go test -mod=vendor ./...

vet:
	go vet -mod=vendor ./...

generate:
	go run -mod=vendor ./cmd/lipsync generate -out artifacts -seed 20261126

demo:
	go run -mod=vendor ./cmd/lipsync demo -out artifacts -seed 20261126 -workers 2 -worker-buffer 1 -queue-budget 1
