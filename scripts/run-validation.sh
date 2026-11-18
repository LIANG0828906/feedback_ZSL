#!/usr/bin/env bash
set -euo pipefail

export GOMAXPROCS="${GOMAXPROCS:-2}"
go test ./...
go run ./cmd/validate \
  -samples samples/gateways.json \
  -out artifacts/validation-report.json \
  -work-dir artifacts/validation-run \
  -gomaxprocs 2 \
  -memory-limit-mib 1250
