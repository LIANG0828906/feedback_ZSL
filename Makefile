.PHONY: test verify serve build fmt

test:
	go test -mod=vendor -count=1 ./...

verify:
	go run -mod=vendor ./cmd/enginelab verify -out artifacts

serve:
	go run -mod=vendor ./cmd/enginelab serve -addr 127.0.0.1:8088 -db artifacts/workbench.sqlite

build:
	go build -mod=vendor -o artifacts/enginelab ./cmd/enginelab

fmt:
	gofmt -w cmd internal acceptance_test.go
