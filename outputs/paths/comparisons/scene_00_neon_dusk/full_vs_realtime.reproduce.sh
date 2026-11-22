#!/usr/bin/env bash
set -euo pipefail
cd "/home/liang/feedback/uid42a8691e-default-154"
.venv/bin/python -m mshdr doctor
.venv/bin/python -m mshdr generate --seed 20261123 --out samples
.venv/bin/python -m mshdr run-paths --dataset samples/dataset.json --out outputs/paths --checkpoint-root checkpoints/paths
.venv/bin/python -m mshdr compare --left "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/full/scene_00_neon_dusk" --right "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/realtime/scene_00_neon_dusk" --report "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/comparisons/scene_00_neon_dusk/full_vs_realtime.json"
cat "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/comparisons/scene_00_neon_dusk/full_vs_realtime.json"
