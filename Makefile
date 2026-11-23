.PHONY: gen test race vet demo serve build

gen:
	go run ./cmd/storygen -out samples -seed 20261124 -count 3

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

demo:
	go run ./cmd/workbench demo -samples samples -artifacts artifacts/verification -workers 4 -buffer 8

serve:
	go run ./cmd/workbench serve

build:
	go build -o bin/workbench ./cmd/workbench

