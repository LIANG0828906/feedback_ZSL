#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

./scripts/bootstrap_offline.sh
mkdir -p data reports

.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m causalkernel.cli experiment \
  --data-dir data \
  --reports-dir reports \
  --seed 20261121

echo
echo "Validation artifacts:"
echo "  reports/RESULTS.md"
echo "  reports/validation_report.json"
