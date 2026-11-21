#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHON="${PYTHON:-$ROOT/.venv/bin/python}"
WORKSPACE="${1:-$ROOT/artifacts/demo}"

cd "$ROOT"
"$PYTHON" -m unittest discover -s tests -v
"$PYTHON" -m acoustic_workbench.cli diagnose --workspace "$WORKSPACE"

