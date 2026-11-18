#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -x .venv/bin/python ]]; then
  echo "Run: python3.11 -m venv .venv && .venv/bin/pip install -r requirements.txt"
  exit 1
fi

PY=.venv/bin/python
$PY scripts/generate_assets.py --output-dir generated --samples 10000
$PY -m pytest -q
mkdir -p reports
$PY scripts/run_scenario.py --tenant tenant-a --protocol-version v3 --count 5000 --rate 0 --canary-weight 0.0 --timeout-seconds 15 --output reports/workload_v3_5000.json >/dev/null
$PY scripts/run_scenario.py --tenant tenant-d --protocol-version v4 --count 5000 --rate 0 --canary-weight 1.0 --timeout-seconds 15 --output reports/workload_v4_5000.json >/dev/null
$PY scripts/run_scenario.py --tenant tenant-d --protocol-version mixed --count 120 --rate 200 --canary-weight 0.5 --timeout-seconds 15 --output reports/scenario_mixed.json >/dev/null
$PY scripts/run_scenario.py --tenant tenant-d --protocol-version v4 --count 20 --rate 200 --canary-weight 1.0 --misalign-rate 1.0 --timeout-seconds 15 --output reports/scenario_misalign.json >/dev/null
$PY scripts/run_scenario.py --tenant tenant-c --protocol-version v3 --count 50 --rate 200 --canary-weight 1.0 --inject-load-failure --timeout-seconds 15 --output reports/scenario_load_failure.json >/dev/null
$PY scripts/run_scenario.py --tenant tenant-d --protocol-version v4 --count 80 --rate 100 --canary-weight 1.0 --delay-rate 1.0 --delay-ms 150 --timeout-seconds 15 --output reports/scenario_rollback.json >/dev/null
$PY scripts/run_scenario.py --tenant tenant-d --protocol-version v4 --count 45 --rate 100 --canary-weight 1.0 --delay-rate 1.0 --delay-ms 700 --timeout-seconds 15 --output reports/scenario_timeout.json >/dev/null
echo "Validation complete. Reports are in reports/."
