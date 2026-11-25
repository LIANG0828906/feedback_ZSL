.PHONY: test test-race vet demo serve build wails

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

demo:
	go run ./cmd/terrainbake demo -seed 424242

serve:
	go run ./cmd/terrainbake serve -seed 424242 -addr 127.0.0.1:8090

build:
	mkdir -p bin
	go build -o bin/terrainbake ./cmd/terrainbake

wails:
	cd wailsapp && wails build -tags webkit2_41
