.PHONY: build test race samples accept gui clean

build:
	go build ./...

test:
	go test ./...

race:
	go test -race ./...

samples:
	go run ./cmd/workspace generate -out samples -seed 20261124

accept:
	go run ./cmd/workspace accept \
		-samples samples \
		-db artifacts/expression-checkpoints.db \
		-report artifacts/acceptance.json \
		-markdown artifacts/ACCEPTANCE.md

gui:
	go build -tags gui -o workspace-desktop ./cmd/desktop

clean:
	rm -f workspace-desktop workspace-desktop.exe
	rm -f artifacts/*.db artifacts/*.db-wal artifacts/*.db-shm
