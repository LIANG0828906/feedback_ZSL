#!/usr/bin/env bash
# One-shot acceptance entry: environment check + full verification suite.
set -euo pipefail
cd "$(dirname "$0")/.."
PY="${PYTHON:-.venv/bin/python}"
"$PY" scripts/acceptance.py
