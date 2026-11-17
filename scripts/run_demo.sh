#!/usr/bin/env bash
# Deterministic end-to-end demo: generate -> render -> resume verification.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
MANIFEST="configs/demo_manifest.json"

echo "[1/4] Generating six deterministic microphone tracks..."
python3 -m stage_noise.cli generate "$MANIFEST"

echo "[2/5] Graceful partial render (stops after 8 chunks)..."
python3 -m stage_noise.cli render "$MANIFEST" --interrupt-after-chunk 7

echo "[3/5] Resume to completion (resumed master)..."
python3 -m stage_noise.cli render "$MANIFEST"
cp configs/master_stereo.wav /tmp/stage_master_resumed.wav

echo "[4/5] Regenerate identical inputs, then clean one-shot render..."
python3 -m stage_noise.cli generate "$MANIFEST"
python3 -m stage_noise.cli render "$MANIFEST"

echo "[5/5] Byte-level comparison (resumed vs one-shot)..."
python3 -m stage_noise.cli compare /tmp/stage_master_resumed.wav \
    configs/master_stereo.wav
python3 -m stage_noise.cli compare /tmp/stage_master_reference.wav \
    configs/master_stereo.wav

echo "Demo complete. See configs/noise_report.json for the full report."
