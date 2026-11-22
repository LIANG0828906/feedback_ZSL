#!/usr/bin/env bash
set -euo pipefail
cd "/home/liang/feedback/uid42a8691e-default-154"
.venv/bin/python -m mshdr doctor
.venv/bin/python -m mshdr generate --seed 20261123 --out samples
.venv/bin/python -m mshdr run-paths --dataset samples/dataset.json --out outputs/paths --checkpoint-root checkpoints/paths
.venv/bin/python -m mshdr compare --left "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/full/scene_02_spectral_lab" --right "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/resume/scene_02_spectral_lab" --report "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/comparisons/scene_02_spectral_lab/full_vs_resume.json"
cat "/home/liang/feedback/uid42a8691e-default-154/outputs/paths/comparisons/scene_02_spectral_lab/full_vs_resume.json"
