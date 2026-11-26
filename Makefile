.PHONY: test race vet samples verify workbench

test:
	go test ./...

race:
	go test -race ./dialogue

vet:
	go vet ./...

samples:
	go run ./cmd/samplegen -out samples -seed 42

verify:
	go run ./cmd/verify -seed 42

workbench:
	go run ./cmd/dialogue-workbench -addr 127.0.0.1:8080 -sample samples/dialogue-v3.json -checkpoint artifacts/workbench.checkpoint.json
