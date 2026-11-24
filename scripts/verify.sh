#!/usr/bin/env bash
set -euo pipefail

# The checkpoint driver is modernc.org/sqlite, a pure-Go SQLite implementation.
# Keeping CGO disabled makes the verification independent of local C wrappers.
export CGO_ENABLED=0

echo "== go env =="
go env GOVERSION CGO_ENABLED GOOS GOARCH

echo "== go build =="
go build ./...

echo "== go vet =="
go vet ./...

echo "== go test =="
go test -count=1 ./...

echo "== deterministic sample =="
go run ./cmd/terrain-occlusion generate -out samples/project.json

echo "== three-path verification =="
go run ./cmd/terrain-occlusion verify -in samples/project.json -artifacts artifacts

echo "OK: build, vet, tests, sample and three-path verification passed."
