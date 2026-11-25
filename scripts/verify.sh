#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export GOPROXY=off
export GOFLAGS=-mod=vendor

echo "== go version =="
go version

echo "== go test =="
go test ./...

echo "== build CLI =="
mkdir -p artifacts/bin
go build -o artifacts/bin/plancache ./cmd/plancache

echo "== deterministic schema samples =="
./artifacts/bin/plancache samples --dir artifacts/samples

echo "== pipeline JSON report =="
./artifacts/bin/plancache report --out artifacts/pipeline-report.json >/dev/null

echo "== checkpoint smoke test =="
CHECKPOINT_DIR="$(mktemp -d)"
trap 'rm -rf "$CHECKPOINT_DIR"' EXIT
./artifacts/bin/plancache checkpoint --db "$CHECKPOINT_DIR/checkpoint.db" >/dev/null

echo "OK: tests, build, samples, report and SQLite checkpoint smoke test passed"
