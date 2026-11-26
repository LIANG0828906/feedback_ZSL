.PHONY: test vet demo crash-demo build

test:
	go test ./...

vet:
	go vet ./...

demo:
	go run ./cmd/slikernel -mode demo -seed 20261127 -out artifacts

crash-demo:
	go run ./cmd/slikernel -mode crash-demo -seed 20261127 -out artifacts

build:
	go build -o bin/slikernel ./cmd/slikernel
