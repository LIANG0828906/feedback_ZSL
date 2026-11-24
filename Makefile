.PHONY: generate test race verify run desktop

generate:
	go run ./cmd/samplegen -out samples -seed 42

test:
	go test ./...
	go vet ./...

race:
	go test -race ./pkg/...

verify:
	go run ./cmd/verify -out artifacts/verification.json

run: generate
	go run ./cmd/volumelab -manifest samples/manifest.json -session-dir samples/sessions

desktop:
	cd desktop && wails build -s
