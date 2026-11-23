#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

if [ ! -x ".venv/bin/python" ]; then
  uv venv --python 3.11 .venv
  uv pip install --python .venv/bin/python -r requirements.txt
fi

.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python examples/run_demo.py

echo "All checks finished."
