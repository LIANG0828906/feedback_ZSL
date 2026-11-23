.PHONY: test race verify samples lab desktop clean

test:
	go test ./...

race:
	go test -race ./internal/lab ./internal/pipeline

verify:
	go run ./cmd/verify -samples samples -seed 20261124

samples:
	go run ./cmd/samplegen -dir samples -seed 20261124

lab:
	go run ./cmd/ocrlab -addr 127.0.0.1:8123 -samples samples -db data/workbench.db

desktop:
	wails build -clean

clean:
	rm -rf data build/bin
