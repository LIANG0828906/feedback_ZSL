.DEFAULT_GOAL := verify

GOPROXY ?= https://goproxy.cn,direct
export GOPROXY

.PHONY: help tidy fmt test race verify generate build cgo0 kill-demo

help:
	@echo "make verify   # generate sample and run full local acceptance"
	@echo "make test     # go test ./..."
	@echo "make race     # race-enabled focused tests"
	@echo "make cgo0     # pure-Go SQLite build with CGO_ENABLED=0"
	@echo "make kill-demo # hard-exit after checkpoint and resume"

tidy:
	GOPROXY=https://goproxy.cn,direct go mod tidy

fmt:
	gofmt -w .

test:
	go test ./...

race:
	go test -race ./engine ./schema ./demo

verify:
	go run ./cmd/alertdemo all --out artifacts/demo

generate:
	go run ./cmd/alertdemo generate --out artifacts/demo

build:
	go build -o bin/alertdemo ./cmd/alertdemo

cgo0:
	CGO_ENABLED=0 go build ./...

kill-demo: build
	@db="artifacts/demo/checkpoints/kill-make.db"; \
	./bin/alertdemo crash-run --db $$db --crash-after 2 || test $$? -eq 137; \
	./bin/alertdemo resume --db $$db
