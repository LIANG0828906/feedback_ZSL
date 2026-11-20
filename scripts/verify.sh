#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ ! -x .venv/bin/python ]]; then
  uv python install 3.11
  uv venv --python 3.11 .venv
  uv pip install --python .venv/bin/python -r requirements.txt
fi

.venv/bin/python -m pytest -q

A="$(mktemp -d /tmp/hdr-delivery-a.XXXXXX)"
B="$(mktemp -d /tmp/hdr-delivery-b.XXXXXX)"

.venv/bin/python -m hdr_studio.cli demo --workspace "$A" --force >/dev/null
.venv/bin/python -m hdr_studio.cli demo --workspace "$B" --force >/dev/null

diff -r "$A" "$B"

echo "deterministic workspaces:"
echo "  A=$A"
echo "  B=$B"
echo "trace SHA-256:"
sha256sum "$A/trace/trace.jsonl" "$B/trace/trace.jsonl"
echo "baseline SHA-256:"
sha256sum "$A/baselines.json" "$B/baselines.json"

QT_QPA_PLATFORM=offscreen HDR_STUDIO_AUTOCLOSE_MS=300 \
  .venv/bin/python -m hdr_studio.cli workbench --workspace "$A" --offscreen

echo "verification: PASS"

