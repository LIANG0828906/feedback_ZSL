#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHON="${ROOT}/.venv/bin/python"
OUT="${1:-${ROOT}/validation/demo}"

if [[ ! -x "${PYTHON}" ]]; then
  echo "missing ${PYTHON}; create .venv first" >&2
  exit 2
fi

"${PYTHON}" -m studio_calibrator generate --out "${OUT}/sample" --seed 42 --duration 1.0 --force
"${PYTHON}" -m studio_calibrator validate \
  --manifest "${OUT}/sample/manifest.json" \
  --geometry "${OUT}/sample/geometry.json" \
  --config "${OUT}/sample/config.json"
"${PYTHON}" -m studio_calibrator diagnose \
  --manifest "${OUT}/sample/manifest.json" \
  --geometry "${OUT}/sample/geometry.json" \
  --config "${OUT}/sample/config.json" \
  --snapshot-dir "${OUT}/snapshots" \
  --trace "${OUT}/trace.jsonl" \
  --out "${OUT}/diagnosis.json"
"${PYTHON}" -m studio_calibrator restore \
  --snapshot-dir "${OUT}/snapshots" \
  --out "${OUT}/restored.json"
