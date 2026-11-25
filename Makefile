.PHONY: test race vet generate validate demo faults all clean

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

generate:
	go run ./cmd/graphlab generate -seed 42 -out artifacts/sample-seed42

validate:
	go run ./cmd/graphlab validate -sample artifacts/sample-seed42 -out artifacts/validation -workers 4 -buffer 4096

demo:
	go run ./cmd/graphlab demo -sample artifacts/sample-seed42 -checkpoint artifacts/demo/checkpoint.db -out artifacts/demo -workers 4 -buffer 4096

faults:
	go run ./cmd/graphlab faults -sample artifacts/sample-seed42 -out artifacts -workers 4 -buffer 4096

all: generate validate demo faults

clean:
	rm -rf artifacts/demo artifacts/validation artifacts/faults artifacts/faults-report.json
