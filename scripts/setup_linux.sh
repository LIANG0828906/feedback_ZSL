#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
PYTHON="${PYTHON:-python3}"

# Core algorithms only need the standard library. Create an isolated virtual
# environment from the local interpreter only when venv/pip tooling is present;
# otherwise reuse the local offline interpreter directly.
if "$PYTHON" -m venv .venv >/dev/null 2>&1 && [ -x .venv/bin/python ]; then
  . .venv/bin/activate
  python -m pip install --no-index --find-links wheelhouse -r requirements.txt ||
    printf 'wheelhouse install skipped; continuing with the local offline interpreter\n'
  RUNTIME=python
else
  printf 'venv unavailable; using the local offline interpreter: %s\n' "$("$PYTHON" -c 'import sys; print(sys.executable)')"
  RUNTIME="$PYTHON"
fi
"$RUNTIME" -m mediaforge.samplegen --out samples --seed 20261123
"$RUNTIME" -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
printf '\nStart the workbench with:\n  %s run_workbench.py\n' "$RUNTIME"
