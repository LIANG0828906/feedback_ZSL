.PHONY: test vet race validate workbench clean

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

validate:
	go run ./cmd/validate -out artifacts/validation

workbench:
	go run ./cmd/workbench -addr 127.0.0.1:8099 -db artifacts/workbench/checkpoints.db -out artifacts/workbench

clean:
	rm -rf artifacts/validation artifacts/workbench
