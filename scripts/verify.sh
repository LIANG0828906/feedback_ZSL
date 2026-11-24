#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

export CGO_ENABLED=0
go test ./...
go run ./cmd/scene-gen -seed 1042 -out data/projects/seed-1042 -tolerance-px 2
go run ./cmd/scene-gen -seed 1043 -out data/projects/seed-1043 -tolerance-px 2
go run ./cmd/scene-gen -seed 1044 -out data/projects/seed-1044 -tolerance-px 2
go run ./cmd/verify -seed 1042 -raster 128 -workers 2 \
  -out artifacts/validation-report.json
go run ./cmd/verify -seed 1043 -raster 128 -workers 2 \
  -out artifacts/validation-report-1043.json
go run ./cmd/verify -seed 1044 -raster 128 -workers 2 \
  -out artifacts/validation-report-1044.json
if command -v node >/dev/null 2>&1; then
  node --check frontend/dist/app.js
else
  echo "node not found; skipped JavaScript syntax check"
fi
