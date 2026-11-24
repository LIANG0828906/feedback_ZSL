#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:--mod=vendor}"

ADDR="${ADDR:-127.0.0.1:18080}"
SAMPLE="${SAMPLE:-samples/terrain-seed-42.json}"
DB="${DB:-artifacts/terrain-checkpoint.db}"

scripts/generate-sample.sh 42 "$SAMPLE"
mkdir -p "$(dirname "$DB")"
exec go run ./cmd/terrain-server -addr "$ADDR" -sample "$SAMPLE" -db "$DB"
