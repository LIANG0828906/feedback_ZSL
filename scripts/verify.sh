#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

mkdir -p artifacts
LOG="artifacts/verification.log"
{
  echo "# verification $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "\$ go version"
  go version
  echo
  echo "\$ go test ./..."
  go test ./... -count=1
  echo
  echo "\$ go test -race ./..."
  go test -race ./... -count=1
  echo
  echo "\$ go vet ./..."
  go vet ./...
  echo
  echo "\$ go run ./cmd/quotademo -seed 42 -workers 4 -queue 64 -commit-stop 2 -artifacts artifacts/demo"
  go run ./cmd/quotademo -seed 42 -workers 4 -queue 64 -commit-stop 2 -artifacts artifacts/demo
  for injection in \
    negative-quota \
    guarantee-over-capacity \
    quota-scale-overflow \
    unknown-schema \
    invalid-budget \
    queue-backpressure \
    truncated-checkpoint \
    version-mismatch; do
    echo
    echo "\$ go run ./cmd/quotademo -inject ${injection}"
    go run ./cmd/quotademo -inject "$injection"
  done
} | tee "$LOG"
