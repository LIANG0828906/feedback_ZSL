.PHONY: test race vet demo generate build-cli build-desktop workbench-test windows-artifact

test:
	go test ./... -count=1
	cd desktop && go test ./internal/workbench -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

demo:
	go run ./cmd/hdrproof demo --root artifacts/demo --workers 4 --buffer 2

generate:
	go run ./cmd/hdrproof generate --out artifacts/demo/samples --seed 42

build-cli:
	go build -o build/hdrproof ./cmd/hdrproof

build-desktop:
	cd desktop && wails build -platform windows/amd64 -skipbindings

workbench-test:
	cd desktop && go test ./internal/workbench -count=1

windows-artifact:
	cd desktop && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../build/hdrproof-workbench.exe .
