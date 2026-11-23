#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PYTHONPATH=src .venv/bin/python -m unittest discover -s tests -t . -v
.venv/bin/padweave selfcheck --samples samples --artifacts artifacts
PYTHONPATH=src .venv/bin/python scripts/capture_ui_demo.py

echo "Validation artifacts:"
echo "  artifacts/validation_report.json"
echo "  artifacts/validation_summary.md"
echo "  artifacts/ui_dashboard.png"
echo "  artifacts/ui_incremental.png"
echo "  artifacts/ui_dependency.png"
echo "  artifacts/ui_diff_heatmap.png"

