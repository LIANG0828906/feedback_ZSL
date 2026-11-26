GO ?= go
export GOFLAGS := -mod=vendor

.PHONY: all build build-workbench build-samples test race vet vendor samples selfcheck serve verify

all: verify build

build: build-workbench build-samples

build-workbench:
	$(GO) build -mod=vendor -o bin/dns-workbench ./cmd/dns-workbench

build-samples:
	$(GO) build -mod=vendor -o bin/gen-samples ./cmd/gen-samples

test:
	$(GO) test -mod=vendor ./...

race:
	CGO_ENABLED=1 $(GO) test -race -mod=vendor ./...

vet:
	$(GO) vet -mod=vendor ./...

vendor:
	$(GO) mod tidy
	$(GO) mod vendor

samples:
	$(GO) run -mod=vendor ./cmd/gen-samples -seed 42 -out samples

selfcheck:
	$(GO) run -mod=vendor ./cmd/dns-workbench --selfcheck

serve:
	$(GO) run -mod=vendor ./cmd/dns-workbench -addr 127.0.0.1:8080 -seed 42 -db .runtime/dns-checkpoint.sqlite

verify: vet test
	$(GO) run -mod=vendor ./cmd/dns-workbench --selfcheck
