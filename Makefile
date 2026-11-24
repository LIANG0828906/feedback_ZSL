GO ?= go

.PHONY: sample test test-race render verify serve

sample:
	$(GO) run ./cmd/samplegen -out samples -seed 42

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

render:
	$(GO) run ./cmd/render -samples samples -checkpoints checkpoints -out out

verify:
	$(GO) run ./cmd/verify -samples samples -checkpoints checkpoints

serve:
	$(GO) run ./cmd/server -addr 127.0.0.1:8080 -samples samples -checkpoints checkpoints
