#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "$ROOT/.venv/bin/python" "$ROOT/scripts/run_acceptance.py" \
  --config "${1:-$ROOT/config.local.json}" \
  --report "$ROOT/artifacts/acceptance.json"

