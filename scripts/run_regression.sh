#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="${1:-${ROOT}/build-headless}"

cmake -S "${ROOT}" -B "${BUILD_DIR}" \
    -DMESHEDITOR_BUILD_GUI=OFF \
    -DCMAKE_BUILD_TYPE=Release
cmake --build "${BUILD_DIR}" -j "$(nproc)"

"${BUILD_DIR}/mesheditor_tests"
if command -v ctest >/dev/null 2>&1; then
    ctest --test-dir "${BUILD_DIR}" --output-on-failure
fi

"${BUILD_DIR}/mesheditor_cli" regression "${ROOT}"
"${BUILD_DIR}/mesheditor_cli" regression "${ROOT}/.regression_second"

diff -r "${ROOT}/samples" "${ROOT}/.regression_second/samples"
diff -r "${ROOT}/logs" "${ROOT}/.regression_second/logs"

echo "Deterministic regression: BYTE-IDENTICAL"
sha256sum \
    "${ROOT}/logs/regression_trace.jsonl" \
    "${ROOT}/logs/validation_report.json" \
    "${ROOT}/logs/session.mep" \
    "${ROOT}/samples/cube_v2.obj" \
    "${ROOT}/samples/cube_binary.stl"
