.PHONY: test build windows scenario check clean

GO ?= go

test:
	$(GO) test ./...

build:
	mkdir -p artifacts/bin
	$(GO) build -trimpath -o artifacts/bin/edge-gateway ./cmd/edge-gateway
	$(GO) build -trimpath -o artifacts/bin/loadgen ./cmd/loadgen

windows:
	mkdir -p artifacts/windows
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o artifacts/windows/edge-gateway.exe ./cmd/edge-gateway
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o artifacts/windows/loadgen.exe ./cmd/loadgen

scenario:
	python3 scripts/run_scenario.py

check:
	python3 scripts/check_report.py

clean:
	rm -rf artifacts/run artifacts/bin
