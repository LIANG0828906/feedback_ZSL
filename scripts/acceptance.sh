#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

echo "== gofmt =="
gofmt -w $(find . -path ./wailsapp -prune -o -name '*.go' -print)

echo "== go vet =="
go vet ./...

echo "== go test =="
go test -count=1 ./...

echo "== go build =="
go build ./...

OUT="artifacts/local-acceptance"
mkdir -p "$OUT"

echo "== generate deterministic samples =="
go run ./cmd/iconcut generate -out "$OUT/samples"

echo "== run icon-192 =="
go run ./cmd/iconcut run \
  -bundle "$OUT/samples/icon-192" \
  -db "$OUT/icon-192.db" \
  -out "$OUT/out"

echo "== independent verifier =="
go run ./cmd/verifycut -db "$OUT/icon-192.db"

if [[ -d "artifacts/acceptance/icon-192" ]]; then
  echo "== verify supplied compatibility sample =="
  go run ./cmd/iconcut run \
    -bundle "artifacts/acceptance/icon-192" \
    -db "$OUT/supplied-icon-192.db" \
    -out "$OUT/supplied-out"
  go run ./cmd/verifycut -db "$OUT/supplied-icon-192.db"
fi

echo "ACCEPTANCE_OK"
