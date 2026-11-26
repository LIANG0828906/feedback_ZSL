#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
go build -o bin/strokediag ./cmd/strokediag

bin/strokediag generate -seed 424242 -out artifacts/hard-stop-demo
rm -f artifacts/hard-stop-demo.ckpt

set +e
bin/strokediag run \
  -dir artifacts/hard-stop-demo/v3-grouped-128 \
  -mode incremental \
  -checkpoint artifacts/hard-stop-demo.ckpt \
  -hard-abort-after 3 \
  -out artifacts/hard-stop-partial
status=$?
set -e

if [[ "$status" -ne 17 ]]; then
  echo "expected exit status 17, got $status" >&2
  exit 1
fi

bin/strokediag run \
  -dir artifacts/hard-stop-demo/v3-grouped-128 \
  -mode resume \
  -checkpoint artifacts/hard-stop-demo.ckpt \
  -out artifacts/hard-stop-resumed
