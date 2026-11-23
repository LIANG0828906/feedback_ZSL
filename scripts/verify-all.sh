#!/usr/bin/env bash
set -euo pipefail

go version
go test ./...
go vet ./...

go run ./cmd/gaitretarget generate --seed 42 --out ./data
go run ./cmd/gaitretarget validate --data ./data
go run ./cmd/gaitretarget run --mode full --data ./data --source src-walk-001 --target target-compact-01 --out ./artifacts/full-result.json --mapping-out ./artifacts/mapping.json
go run ./cmd/gaitretarget verify --data ./data --source src-walk-001 --target target-compact-01 --store ./artifacts/verify-main --interrupt-after 2 --out ./artifacts/verification-report.json

while read -r source target; do
  go run ./cmd/gaitretarget verify --data ./data --source "$source" --target "$target" \
    --store "./artifacts/verify-$source-$target" --interrupt-after 1
done <<'PAIRS'
src-walk-001 target-compact-01
src-walk-001 target-human-02
src-walk-001 target-stylized-03
src-walk-002 target-compact-01-s2
src-walk-002 target-human-02-s2
src-walk-002 target-stylized-03-s2
PAIRS
