#!/usr/bin/env bash
# Independent verifier: read-only recomputation from persisted blocks + truth.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

build_all
"${ROOT}/bin/verify" \
  -data "${DATA_DIR}" \
  -store "${STORE_ROOT:-${ROOT}/store}/realtime" \
  -quality "${QUALITY}"
