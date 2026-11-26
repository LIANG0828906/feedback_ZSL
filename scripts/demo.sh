#!/usr/bin/env bash
set -euo pipefail

ROOT="${1:-out/demo}"

echo "== go version =="
go version

echo "== unit/integration tests =="
go test -count=1 ./...

echo "== deterministic three-path demo =="
go run ./cmd/reverb demo -out "$ROOT"

echo "== independent comparison =="
go run ./cmd/verify compare "$ROOT/full" "$ROOT/realtime" "$ROOT/recovery" >/tmp/acoustic-compare.json

echo "== independent audits =="
go run ./cmd/verify audit "$ROOT/full" >/tmp/acoustic-audit-full.json
go run ./cmd/verify audit "$ROOT/realtime" >/tmp/acoustic-audit-realtime.json
go run ./cmd/verify audit "$ROOT/recovery" >/tmp/acoustic-audit-recovery.json

echo "OK: report=$ROOT/report.json"
echo "Serve with: go run ./cmd/reverb serve -report $ROOT/report.json -addr 127.0.0.1:8088"
