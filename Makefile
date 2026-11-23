.PHONY: test race vet desktop cli verify

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

desktop:
	wails build

cli:
	go build -o build/stagecue ./cmd/stagecue

verify:
	./scripts/verify-samples.sh
