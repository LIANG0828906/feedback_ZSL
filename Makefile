.PHONY: build vet test race verify windows run

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./pkg/workbench

verify: build vet test race windows

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...

run:
	go run ./cmd/workbench -addr 127.0.0.1:8080 -data ./data
