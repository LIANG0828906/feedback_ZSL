#!/usr/bin/env bash
# Realtime path: fresh checkpoint, bounded worker pool, durable per-block commits.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

build_all
generate_samples
"${ROOT}/bin/pipeline" \
  -data "${DATA_DIR}" \
  -store "${STORE_ROOT}/realtime" \
  -quality "${QUALITY}" \
  -mode realtime \
  -workers "${WORKERS}" \
  -window "${WINDOW}" -stride "${STRIDE}" -halo "${HALO}"
