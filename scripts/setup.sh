#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if command -v uv >/dev/null 2>&1; then
  uv python install 3.11
  uv venv --python 3.11 .venv
else
  python3.11 -m venv .venv
fi

.venv/bin/python -m pip install --upgrade pip
.venv/bin/python -m pip install --no-build-isolation -e ".[test]"
mkdir -p samples cache reports

echo "Environment ready. Activate with: source .venv/bin/activate"

