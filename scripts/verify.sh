#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== go version =="
go version

echo "== go vet =="
go vet ./...

echo "== unit/integration tests =="
go test -count=1 ./...

echo "== deterministic sample + acceptance =="
go run ./cmd/acoustics verify --root .

echo "OK: artifacts/acceptance-report.json"
