#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python3 -m venv .venv
. .venv/bin/activate
python -m pip install --no-index --find-links wheelhouse -r requirements.txt
python -m mediaforge.samplegen --out samples --seed 20261123
python -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
printf '\nStart the workbench with:\n  .venv/bin/python run_workbench.py\n'
