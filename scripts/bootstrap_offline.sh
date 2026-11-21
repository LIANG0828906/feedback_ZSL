#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 WHEELHOUSE [PYTHON]" >&2
  exit 2
fi

WHEELHOUSE="$(cd "$1" && pwd)"
PYTHON_BIN="${2:-python3.11}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENV="${ROOT}/.venv-offline"

"${PYTHON_BIN}" - <<'PY'
import sys
if sys.version_info[:2] != (3, 11):
    raise SystemExit(f"Python 3.11 required, got {sys.version.split()[0]}")
print("Python", sys.version.split()[0])
PY

find "${WHEELHOUSE}" -maxdepth 1 -type f \( -name 'numpy-*.whl' -o -name 'PySide6-*.whl' \) -print
"${PYTHON_BIN}" -m venv "${VENV}"
"${VENV}/bin/python" -m pip install --no-index --find-links "${WHEELHOUSE}" "numpy>=1.26,<3" "PySide6>=6.6,<7"
"${VENV}/bin/python" - <<'PY'
import numpy, PySide6
print("NumPy", numpy.__version__)
print("PySide6", PySide6.__version__)
PY
echo "Offline environment ready: ${VENV}"
echo "Run: ${VENV}/bin/python -m tileforge --help"
