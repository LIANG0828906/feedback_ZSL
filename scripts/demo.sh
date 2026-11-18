#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -x .venv/bin/python ]]; then
  python3 -m venv .venv
fi
.venv/bin/python -m pip install -r requirements.txt

.venv/bin/python -m minute_backfill.cli generate \
  --out data/demo --seed 42 --payment-rows 100000 --refund-rows 40000 --batches 128

.venv/bin/python -m minute_backfill.cli run \
  --data data/demo --state state/demo.db --out out/demo \
  --slots 6 --workers 2 --allowed-lateness-seconds 60

echo "Report: out/demo/report.json"
