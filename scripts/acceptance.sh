#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:--mod=vendor}"

echo "== go version =="
go version
echo "== go env GOMODCACHE =="
go env GOMODCACHE
test -w "$(go env GOMODCACHE)" && echo "GOMODCACHE is writable"

echo "== generate deterministic sample =="
scripts/generate-sample.sh 42 samples/terrain-seed-42.json

echo "== go test =="
go test ./...

echo "== local acceptance: schema, mesh, attribution, incremental, recovery, SQLite =="
go run ./cmd/terrain-acceptance -dir artifacts/acceptance

echo "DONE. See artifacts/acceptance/acceptance-report.json"
