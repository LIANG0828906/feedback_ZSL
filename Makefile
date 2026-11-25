.PHONY: test demo serve verify

test:
	go test ./... -count=1

demo:
	go run ./cmd/acoustic demo

serve:
	go run ./cmd/acoustic serve

verify:
	go run ./cmd/acoustic verify

