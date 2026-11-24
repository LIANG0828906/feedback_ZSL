#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
export CGO_ENABLED=0

go vet ./...
go test -count=1 ./...
go build -o artifacts/integrity-diagnose ./cmd/diagnose
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -o artifacts/integrity-diagnose-windows-amd64.exe ./cmd/diagnose

./artifacts/integrity-diagnose gen -sample samples/default
./artifacts/integrity-diagnose crosscheck \
  -sample samples/default \
  -artifacts artifacts \
  -json artifacts/crosscheck.json
./artifacts/integrity-diagnose review \
  -db artifacts/recovery-checkpoint.db \
  -fingerprint samples/default/truth.fingerprint.json \
  -json artifacts/review.json
