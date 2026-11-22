#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${PYTHON_BIN:-}" ]]; then
    "${PYTHON_BIN}" -m venv .venv
    .venv/bin/python -m pip install --upgrade pip
    .venv/bin/python -m pip install -r requirements.txt
    .venv/bin/python -m pip install -e .
elif command -v python3.11 >/dev/null 2>&1; then
    python3.11 -m venv .venv
    .venv/bin/python -m pip install --upgrade pip
    .venv/bin/python -m pip install -r requirements.txt
    .venv/bin/python -m pip install -e .
elif command -v uv >/dev/null 2>&1; then
    uv venv --python 3.11 .venv
    uv pip install --python .venv/bin/python -r requirements.txt
    uv pip install --python .venv/bin/python -e .
else
    echo "Python 3.11 or uv is required." >&2
    exit 1
fi

.venv/bin/python -m transparent_pipeline.cli doctor
.venv/bin/python -m transparent_pipeline.cli generate --out samples --seed 20261123

echo
echo "Environment ready. Next:"
echo "  .venv/bin/python -m pytest -q"
echo "  .venv/bin/python -m transparent_pipeline.cli verify"
echo "  .venv/bin/python -m transparent_pipeline.cli serve"

