#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -z "${PY311:-}" ]]; then
  if command -v python3.11 >/dev/null 2>&1; then
    PY311="$(command -v python3.11)"
  elif command -v uv >/dev/null 2>&1; then
    PY311="$(uv python find 3.11)"
  else
    echo "Python 3.11 or uv is required" >&2
    exit 1
  fi
fi

if [[ ! -d "$ROOT/wheelhouse" ]]; then
  echo "missing local wheelhouse: $ROOT/wheelhouse" >&2
  exit 1
fi

if [[ ! -d "$ROOT/.venv" ]]; then
  uv venv --python "$PY311" "$ROOT/.venv"
fi

uv pip install \
  --python "$ROOT/.venv/bin/python" \
  --no-index \
  --find-links "$ROOT/wheelhouse" \
  --only-binary=:all: \
  "numpy==2.2.6" \
  "PySide6==6.8.3"

"$ROOT/.venv/bin/python" - <<'PY'
import sys
import numpy
import PySide6
print(sys.version.split()[0], "numpy", numpy.__version__, "PySide6", PySide6.__version__)
PY

