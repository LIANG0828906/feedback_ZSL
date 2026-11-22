#!/usr/bin/env bash
set -euo pipefail

PYTHON_BIN="${PYTHON_BIN:-python3.11}"
WHEELHOUSE="${1:-wheelhouse}"

"${PYTHON_BIN}" -m venv .venv

if command -v uv >/dev/null 2>&1; then
  uv pip install --python .venv/bin/python --offline --no-index \
    --find-links "${WHEELHOUSE}" -r requirements.txt
else
  .venv/bin/python -m pip install --no-index \
    --find-links "${WHEELHOUSE}" -r requirements.txt
fi

echo "Offline environment ready: .venv"

