#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
PYTHON="${PYTHON:-/usr/bin/python3}"
export PYTHONPATH="$PWD/.deps${PYTHONPATH:+:$PYTHONPATH}"

"$PYTHON" -c 'import numpy, PySide6'
"$PYTHON" -m mediaforge.samplegen --out samples --seed 20261123
"$PYTHON" -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
printf '\nStart the workbench with:\n  %s run_workbench.py\n' "$PYTHON"
