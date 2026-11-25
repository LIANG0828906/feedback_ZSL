.PHONY: cache generate test race verify diagnose demo tidy

ROOT := $(shell pwd)
export GOMODCACHE := $(ROOT)/.gocache/mod
export GOCACHE := $(ROOT)/.gocache/build

cache:
	mkdir -p $(GOMODCACHE) $(GOCACHE) data/samples data/truth data/checkpoints
	GOPROXY=https://proxy.golang.org,direct go mod download

tidy:
	GOPROXY=https://proxy.golang.org,direct go mod tidy

generate:
	GOPROXY=off go run ./cmd/featurectl generate --data data --seed 20261126 --batches 3

test:
	GOPROXY=off go test ./... -count=1

race:
	GOPROXY=off go test -race ./... -count=1

verify:
	GOPROXY=off go run ./cmd/featurectl verify --data data

diagnose:
	GOPROXY=off go run ./cmd/featurectl diagnose

demo: generate verify
	GOPROXY=off go run ./cmd/featurectl diagnose > data/diagnostic-report.json
	GOPROXY=off go build -o data/checkpoints/featurectl ./cmd/featurectl
	rm -f data/checkpoints/demo.sqlite data/checkpoints/demo.sqlite-wal data/checkpoints/demo.sqlite-shm
	./data/checkpoints/featurectl consume --db data/checkpoints/demo.sqlite --sample data/samples/batch-001.v3.json --batch-size 7
	./data/checkpoints/featurectl snapshot --db data/checkpoints/demo.sqlite --out data/checkpoints/batch-001.snapshot.json --project-id feature-project-001 --batch-id batch-001
