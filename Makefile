.PHONY: all test vet generate verify serve race

all: test vet

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

generate:
	go run ./cmd/dialogue generate -out samples

verify:
	go run ./cmd/dialogue verify -out artifacts

serve:
	go run ./cmd/dialogue serve -addr 127.0.0.1:18086
