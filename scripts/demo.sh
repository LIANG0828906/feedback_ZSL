#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# The module has no third-party dependencies. GOPROXY=off proves the demo does
# not need network access or module downloads.
export GOPROXY=off

echo "== go test =="
go test ./...

echo "== deterministic generator =="
go run ./cmd/leasegen -out samples/generated

echo "== repeatable demo =="
go run ./cmd/leasedemo demo-output

echo "== sqlite file =="
ls -l demo-output/checkpoint/lease.db
