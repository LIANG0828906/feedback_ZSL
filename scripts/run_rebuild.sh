#!/usr/bin/env bash
# Full recompute path: discard any previous checkpoint and rebuild from zero.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

build_all
generate_samples
"${ROOT}/bin/pipeline" \
  -data "${DATA_DIR}" \
  -store "${STORE_ROOT}/rebuild" \
  -quality "${QUALITY}" \
  -mode rebuild \
  -workers 1 \
  -window "${WINDOW}" -stride "${STRIDE}" -halo "${HALO}"
