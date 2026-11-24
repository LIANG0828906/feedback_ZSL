GO ?= go

.PHONY: all
all: test verify

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: tidy
tidy:
	CGO_ENABLED=0 $(GO) mod tidy

.PHONY: test
test:
	CGO_ENABLED=0 $(GO) test -count=1 ./...

.PHONY: samples
samples:
	CGO_ENABLED=0 $(GO) run ./cmd/storyboard generate --out samples

.PHONY: verify
verify:
	CGO_ENABLED=0 $(GO) run ./cmd/storyboard verify --samples samples

.PHONY: negative
negative:
	CGO_ENABLED=0 $(GO) run ./cmd/storyboard negative-checks

.PHONY: serve
serve:
	CGO_ENABLED=0 $(GO) run ./cmd/storyboard serve --addr 127.0.0.1:8787 --samples samples --db data/storyboard.db

.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -o bin/storyboard ./cmd/storyboard

.PHONY: wails-windows
wails-windows:
	CGO_ENABLED=0 wails build -tags wails_desktop -s -skipbindings -nopackage -platform windows/amd64
