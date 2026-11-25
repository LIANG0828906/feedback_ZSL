#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "[1/5] go version"
go version

echo "[2/5] go test"
go test ./... -count=1

echo "[3/5] go vet"
go vet ./...

echo "[4/5] CGO_ENABLED=0 build"
CGO_ENABLED=0 go build -o artifacts/presentkernel ./cmd/presentkernel

echo "[5/5] deterministic samples and acceptance"
./artifacts/presentkernel generate --out samples
./artifacts/presentkernel verify \
  --db artifacts/acceptance-checkpoint.sqlite \
  --report artifacts/acceptance-state.json

echo "OK. Start the local console with:"
echo "  ./artifacts/presentkernel serve --addr 127.0.0.1:8080 --db artifacts/diagnostic.sqlite"
