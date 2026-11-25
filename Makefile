.PHONY: test verify schema ui tidy wails-dev wails-build

test:
	go test ./...

verify:
	go run ./cmd/prosodyctl verify --workdir artifacts/checkpoints --out artifacts/verification.json

schema:
	go run ./cmd/prosodyctl schema-check --out artifacts/schema-check.json

ui:
	go run ./cmd/diag --addr 127.0.0.1:8090 --workdir artifacts/diag

tidy:
	go mod tidy

wails-dev:
	cd wails-app && wails dev

wails-build:
	cd wails-app && wails build
