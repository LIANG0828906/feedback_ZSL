#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin work
go build -o bin/uvtool ./cmd/uvtool

bin/uvtool verify \
  -sample samples/sample_01 \
  -edits configs/fixed_edits.json \
  -workdir work/verify-small \
  -cancel-step 2 \
  -cancel-after-stages 1 \
  -workers 2 \
  -buffer-mib 128 >work/verify-small.json

bin/uvtool verify \
  -sample samples/sample_02 \
  -edits configs/fixed_edits.json \
  -workdir work/verify-large \
  -cancel-step 2 \
  -cancel-after-stages 1 \
  -workers 4 \
  -buffer-mib 128 >work/verify-large.json

echo "three-path verification passed for both committed samples"
echo "reports: work/verify-small/verify_report.json work/verify-large/verify_report.json"
