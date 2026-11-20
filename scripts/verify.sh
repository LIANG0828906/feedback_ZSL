#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p samples cache reports reports/repro-a reports/repro-b
.venv/bin/python -m studio_calib doctor
.venv/bin/python -m studio_calib generate --out samples --seed 20261120 >/tmp/studio-calib-generate.json
.venv/bin/python -m pytest -q

.venv/bin/python -m studio_calib run \
  --wav samples/surround51.wav \
  --cache cache \
  --report reports/diagnostic.json \
  --trace reports/trace.jsonl \
  --coefficients reports/filter_coefficients.json

.venv/bin/python -m studio_calib run \
  --wav samples/surround51.wav \
  --cache reports/repro-a/cache \
  --no-cache \
  --report reports/repro-a/diagnostic.json \
  --trace reports/repro-a/trace.jsonl \
  --coefficients reports/repro-a/filter_coefficients.json >/tmp/studio-calib-repro-a.json

.venv/bin/python -m studio_calib run \
  --wav samples/surround51.wav \
  --cache reports/repro-b/cache \
  --no-cache \
  --report reports/repro-b/diagnostic.json \
  --trace reports/repro-b/trace.jsonl \
  --coefficients reports/repro-b/filter_coefficients.json >/tmp/studio-calib-repro-b.json

cmp reports/repro-a/trace.jsonl reports/repro-b/trace.jsonl
cmp reports/repro-a/filter_coefficients.json reports/repro-b/filter_coefficients.json
echo "Reproducible trace and filter coefficients verified."

