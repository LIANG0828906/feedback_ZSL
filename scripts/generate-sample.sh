#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# modernc.org/sqlite is pure Go; CGO is intentionally disabled so the broken
# local gcc / missing mingw is never required.
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:--mod=vendor}"

SEED="${1:-42}"
OUT="${2:-samples/terrain-seed-42.json}"

mkdir -p "$(dirname "$OUT")"
go run ./cmd/terrain-gen -seed "$SEED" -out "$OUT"
