#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== go version =="
go version

echo "== go test =="
go test -count=1 ./...

echo "== deterministic acceptance =="
go run ./cmd/diagnostic --workdir artifacts/acceptance

echo "== browser fallback build =="
go build -o /tmp/lightprobe-browser .

echo "== Wails tag build (current GOOS) =="
go build -tags wails -o /tmp/lightprobe-wails .

echo "== Windows Wails cross-build =="
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags wails -o /tmp/lightprobe-wails.exe .
echo "GOOS=windows -> $(GOOS=windows go env GOOS)"

if command -v node >/dev/null 2>&1; then
  echo "== frontend syntax =="
  node --check frontend/dist/app.js
fi

echo "All verification steps completed."
