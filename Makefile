.PHONY: test vet demo clean

test:
	go test ./...

vet:
	go vet ./...

demo:
	go run ./cmd/diffrender -seed 20261124 -tasks 3 -out artifacts/demo

clean:
	rm -rf artifacts/demo
