#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
if [[ -x .venv/bin/python ]]; then
  exec .venv/bin/python -m brecon gui --workspace artifacts/demo "$@"
fi
exec python3.11 -m brecon gui --workspace artifacts/demo "$@"

