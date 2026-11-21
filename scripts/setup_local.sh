#!/usr/bin/env bash
set -euo pipefail

# Usage: scripts/setup_local.sh /path/to/wheels
# The wheel directory must contain NumPy, PyOpenGL, glfw, and optionally pytest.
WHEELS="${1:-vendor/wheels}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 -m venv "$ROOT/.venv"
"$ROOT/.venv/bin/python" -m ensurepip --upgrade
"$ROOT/.venv/bin/python" -m pip install \
  --no-index --find-links "$WHEELS" "setuptools>=68" wheel
"$ROOT/.venv/bin/python" -m pip install \
  --no-index --find-links "$WHEELS" -r "$ROOT/requirements.txt"
"$ROOT/.venv/bin/python" -m pip install \
  --no-index --find-links "$WHEELS" --no-build-isolation -e "$ROOT"
