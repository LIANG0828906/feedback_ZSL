GO ?= go
SEED ?= 42
OUT ?= artifacts

.PHONY: all test race vet demo verify clean

all: test demo

test:
	$(GO) test ./... -count=1

race:
	$(GO) test -race ./... -count=1

vet:
	$(GO) vet ./...

demo:
	$(GO) run ./cmd/thermalcache -seed $(SEED) -out $(OUT)

verify: vet test race demo

clean:
	rm -rf $(OUT)/generated $(OUT)/checkpoint.sqlite $(OUT)/partial-checkpoint.sqlite
	rm -f $(OUT)/demo_output.json $(OUT)/verification.txt
