#!/usr/bin/env bash
set -euo pipefail

SAMPLE_DIR="${1:-samples/default}"
SEED="${SEED:-241125}"

go version
go run ./cmd/editorpick generate --dir "$SAMPLE_DIR" --seed "$SEED"
go run ./cmd/editorpick verify --dir "$SAMPLE_DIR" --report "$SAMPLE_DIR/verification-report.json"
go test ./... -count=1

echo "Verification completed successfully."
