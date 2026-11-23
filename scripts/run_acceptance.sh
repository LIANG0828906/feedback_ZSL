#!/usr/bin/env bash
# End-to-end acceptance: generate, run the three paths, inject a crash+resume,
# compare every checkpoint and run the independent verifier. Business errors
# surface as JSON on stderr with the documented integer exit codes.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

build_all
generate_samples

echo "== realtime (parallel) =="
"${ROOT}/bin/pipeline" -data "${DATA_DIR}" -store "${STORE_ROOT}/realtime" \
  -quality "${QUALITY}" -mode realtime -workers "${WORKERS}" \
  -window "${WINDOW}" -stride "${STRIDE}" -halo "${HALO}"

echo "== full rebuild (single thread) =="
"${ROOT}/bin/pipeline" -data "${DATA_DIR}" -store "${STORE_ROOT}/rebuild" \
  -quality "${QUALITY}" -mode rebuild -workers 1 \
  -window "${WINDOW}" -stride "${STRIDE}" -halo "${HALO}"

echo "== crash injection then resume =="
"${ROOT}/bin/pipeline" -data "${DATA_DIR}" -store "${STORE_ROOT}/resume" \
  -quality "${QUALITY}" -mode rebuild -workers "${WORKERS}" -crash-after 40 || true
"${ROOT}/bin/pipeline" -data "${DATA_DIR}" -store "${STORE_ROOT}/resume" \
  -quality "${QUALITY}" -mode resume -workers "${WORKERS}" \
  -window "${WINDOW}" -stride "${STRIDE}" -halo "${HALO}"

echo "== three-path comparison =="
"${ROOT}/bin/compare" -a "${STORE_ROOT}/realtime" -b "${STORE_ROOT}/rebuild"
"${ROOT}/bin/compare" -a "${STORE_ROOT}/realtime" -b "${STORE_ROOT}/resume"

echo "== independent verification =="
"${ROOT}/bin/verify" -data "${DATA_DIR}" -store "${STORE_ROOT}/realtime" -quality "${QUALITY}"
"${ROOT}/bin/verify" -data "${DATA_DIR}" -store "${STORE_ROOT}/resume" -quality "${QUALITY}"
