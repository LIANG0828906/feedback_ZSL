.PHONY: test race vet generate verify serve

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

generate:
	go run ./cmd/stagepreview generate -out samples -seed 42

verify:
	go run ./cmd/stagepreview verify -samples samples -runtime runtime/verify

serve:
	go run ./cmd/stagepreview serve -addr 127.0.0.1:8080 -samples samples -runtime runtime
