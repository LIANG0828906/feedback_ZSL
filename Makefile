.PHONY: build test validation serve clean

build:
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -o bin/acoustic ./cmd/acoustic

test:
	CGO_ENABLED=0 GOTOOLCHAIN=local go test ./...

validation:
	bash scripts/run_validation.sh

serve:
	./bin/acoustic serve --input data/validation/edited.json --db data/validation/acoustic.db --project-out data/validation/served.json --addr 127.0.0.1:18123

windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOTOOLCHAIN=local go build -o bin/acoustic.exe ./cmd/acoustic

clean:
	rm -rf bin data/validation
