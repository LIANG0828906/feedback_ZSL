GO ?= go
SQLITE ?= $(if $(wildcard .tools/root/usr/bin/sqlite3),.tools/root/usr/bin/sqlite3,sqlite3)

.PHONY: generate verify test race vet demo serve db-check all

generate:
	$(GO) run ./cmd/dressbatch generate -seed 42 -count 4 -out samples

verify:
	$(GO) run ./cmd/dressbatch verify -dir samples

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

demo:
	$(GO) run ./cmd/dressbatch demo -out artifacts/demo -seed 42 -count 4

serve:
	$(GO) run ./cmd/dressbatch serve -sample samples/sample_00.json -addr 127.0.0.1:8080

db-check:
	$(SQLITE) artifacts/demo/checkpoints.db \
		'select run_id, status from runs;' \
		'select run_id, commit_seq, batch_id, batch_index from batch_checkpoints order by run_id, commit_seq;'

all: generate verify test demo
