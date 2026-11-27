#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p artifacts/validation

{
  echo "== go version =="
  go version
  echo
  echo "== go test ./... =="
  go test ./...
  echo
  echo "== CGO_ENABLED=0 go test ./... =="
  CGO_ENABLED=0 go test ./...
  echo
  echo "== go vet ./... =="
  go vet ./...
  echo
  if [[ "${VERIFY_RACE:-0}" == "1" ]]; then
    echo "== go test -race ./... =="
    go test -race ./...
    echo
  fi
  echo "== deterministic acceptance =="
  go run ./cmd/validate -out artifacts/validation
} 2>&1 | tee artifacts/validation/checks.txt
