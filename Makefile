.PHONY: all build test race generate demo serve clean fmt vet

GO := go
DATA := ./data
VAR := ./var

all: build

build:
	$(GO) build ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

generate:
	$(GO) run ./cmd/audiobench generate --data $(DATA) --seed 42

demo:
	$(GO) run ./cmd/audiobench demo --data $(DATA) --var $(VAR)

serve:
	$(GO) run ./cmd/audiobench serve --data $(DATA) --var $(VAR) --addr 127.0.0.1:8080

clean:
	find $(DATA) $(VAR) -mindepth 1 -delete 2>/dev/null || true
