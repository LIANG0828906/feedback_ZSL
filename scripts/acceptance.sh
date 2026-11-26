#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== gofmt =="
files=$(git ls-files -- '*.go'; find . -name '*.go' -not -path './.git/*')
echo "$files" | sort -u | xargs gofmt -w

echo "== go test =="
go test ./...

echo "== go test -race =="
go test -race ./...

echo "== CGO_ENABLED=0 build/test =="
CGO_ENABLED=0 go test ./...

echo "== deterministic verify =="
go run ./cmd/iconflow verify

echo "acceptance complete"
