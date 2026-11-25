#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

go version
go test ./...
go run ./cmd/terrainpatch generate -dir samples/local -seed 42
go run ./cmd/terrainpatch verify -samples samples/local -db data/verify.db
