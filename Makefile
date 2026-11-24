GO ?= go
GOFLAGS ?= -tags=netgo
export GOFLAGS

.PHONY: test generate bench serve verify clean

test:
	$(GO) test ./...

generate:
	$(GO) run ./cmd/ocrgen -out data -seed 42

bench:
	$(GO) run ./cmd/ocrbench -data data -db artifacts/bench.db -out artifacts/exports

serve:
	$(GO) run ./cmd/ocrworkbench -data data -db artifacts/workbench.db -addr 127.0.0.1:8099

verify:
	$(GO) run ./cmd/ocrverify -manifest data/manifest.json -realtime artifacts/exports/realtime.json -resume artifacts/exports/resume.json -full artifacts/exports/full.json
	$(GO) run ./cmd/ocrverify -manifest data/manifest.json -db artifacts/bench.db

clean:
	$(GO) clean -testcache
