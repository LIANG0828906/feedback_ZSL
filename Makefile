.PHONY: generate verify serve test race vet build clean

generate:
	go run ./cmd/maskqc generate -seed 20261127 -out samples

verify:
	go run ./cmd/maskqc verify -data samples -report artifacts/validation-report.json

serve:
	go run ./cmd/maskqc serve -data samples -db state/maskqc.db -addr 127.0.0.1:8080

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -o bin/maskqc ./cmd/maskqc

clean:
	rm -rf bin state
