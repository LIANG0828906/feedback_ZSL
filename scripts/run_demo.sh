#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export OMP_NUM_THREADS=1
export MKL_NUM_THREADS=1
exec .venv/bin/python -m mlp_ps.demo --config config/demo.json "$@"
