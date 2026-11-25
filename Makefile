.PHONY: test race demo verify clean

test:
	go test ./...

race:
	go test -race ./...

demo:
	go run ./cmd/quotaflow demo -seed 42 -out artifacts/demo

verify:
	go run ./cmd/quotaflow verify -dir artifacts/demo/samples

clean:
	rm -rf samples artifacts/demo
