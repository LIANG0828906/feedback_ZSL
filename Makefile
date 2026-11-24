GO ?= go
WAILS ?= wails

.PHONY: test verify build wails-build clean

test:
	$(GO) test ./... -count=1

verify:
	$(GO) run ./cmd/verify

build:
	$(GO) build ./...

wails-build:
	./scripts/wails-build-local.sh

clean:
	$(GO) clean
	rm -rf build/bin
