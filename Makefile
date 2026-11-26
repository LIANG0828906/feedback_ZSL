.PHONY: all generate fmt test vet verify build demo clean-demo

all: fmt test build verify

generate:
	go run ./cmd/wardrobe generate --dir samples --seed 20261127

fmt:
	gofmt -w .

test:
	go test -count=1 ./...

vet:
	go vet ./...

build:
	mkdir -p artifacts/bin
	go build -o artifacts/bin/wardrobe ./cmd/wardrobe

verify:
	go run ./cmd/wardrobe verify --dir samples --work artifacts/verify

demo: build
	mkdir -p artifacts/demo
	./artifacts/bin/wardrobe demo --checkpoint artifacts/demo/checkpoint.sqlite > artifacts/demo/demo-report.json
	./artifacts/bin/wardrobe incremental-demo --cache artifacts/demo/cache.json > artifacts/demo/incremental.json

clean-demo:
	find artifacts/demo -type f -delete
