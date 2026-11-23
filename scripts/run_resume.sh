#!/usr/bin/env bash
# Resume path: reuse durable blocks after interruption; mismatch invalidates
# the whole snapshot and triggers a full recompute automatically.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

build_all
generate_samples
"${ROOT}/bin/pipeline" \
  -data "${DATA_DIR}" \
  -store "${STORE_ROOT}/resume" \
  -quality "${QUALITY}" \
  -mode resume \
  -workers "${WORKERS}" \
  -window "${WINDOW}" -stride "${STRIDE}" -halo "${HALO}"
