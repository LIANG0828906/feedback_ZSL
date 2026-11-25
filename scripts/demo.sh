#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SEED="${SEED:-42}"
WORKERS="${WORKERS:-4}"
BUFFER="${BUFFER:-4096}"

echo "== go version =="
go version

echo "== unit/acceptance tests (vendored dependencies) =="
go test -mod=vendor ./...

echo "== generate three schema workspaces =="
go run -mod=vendor ./cmd/sre-workbench generate --seed "$SEED" --dir out/samples

echo "== end-to-end verification =="
go run -mod=vendor ./cmd/sre-workbench verify \
  --seed "$SEED" \
  --workers "$WORKERS" \
  --buffer "$BUFFER" \
  --dir artifacts/verify

echo "== scripted diagnostic workbench =="
go run -mod=vendor ./cmd/sre-workbench demo \
  --seed "$SEED" \
  --workers "$WORKERS" \
  --buffer "$BUFFER" \
  --db artifacts/checkpoints/demo.db | tee artifacts/demo-output.txt

echo "Done. Try interactive mode:"
echo "  go run -mod=vendor ./cmd/sre-workbench repl"
