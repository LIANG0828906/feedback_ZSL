#!/usr/bin/env bash
set -euo pipefail
export PYTHONPATH="${PYTHONPATH:-src}"
python -m audio_sync generate --suite --out artifacts/demo_suite
python -m audio_sync validate --session artifacts/demo_suite/session_01
python -m audio_sync verify \
  --session artifacts/demo_suite/session_01 \
  --checkpoint-root artifacts/runs/three-path-demo \
  --out artifacts/runs/verified
python -m unittest discover -s tests -v

