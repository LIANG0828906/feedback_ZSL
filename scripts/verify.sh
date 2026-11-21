#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD="${1:-${ROOT}/build}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/chunk_preview_verify.XXXXXX")"

cleanup() {
    rm -rf "${WORK}"
}
trap cleanup EXIT

cmake -S "${ROOT}" -B "${BUILD}" -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build "${BUILD}" -j "${JOBS:-2}"
ctest --test-dir "${BUILD}" --output-on-failure

"${BUILD}/scene_gen" \
    --out "${WORK}/sample" \
    --grid-x 7 --grid-y 5 --layers 4 \
    --occluder-density 0.7 --seed 42 \
    --views 4 --width 192 --height 108

"${BUILD}/preview_cli" render \
    --scene "${WORK}/sample/scene.bin" \
    --views "${WORK}/sample/views.txt" \
    --out "${WORK}/rendered" \
    --batch 5 \
    --snapshot "${WORK}/frame.snapshot"

"${BUILD}/preview_cli" recover \
    --scene "${WORK}/sample/scene.bin" \
    --views "${WORK}/sample/views.txt" \
    --snapshot "${WORK}/frame.snapshot" \
    --view-index 3

set +e
"${BUILD}/preview_cli" crash-writer \
    --scene "${WORK}/sample/scene.bin" \
    --views "${WORK}/sample/views.txt" \
    --snapshot "${WORK}/crash.snapshot" \
    --view-index 0
CRASH_STATUS=$?
set -e

if [[ "${CRASH_STATUS}" -ne 137 ]]; then
    echo "expected crash-writer to be killed by SIGKILL (137), got ${CRASH_STATUS}" >&2
    exit 1
fi

"${BUILD}/preview_cli" recover \
    --scene "${WORK}/sample/scene.bin" \
    --views "${WORK}/sample/views.txt" \
    --snapshot "${WORK}/crash.snapshot" \
    --view-index 0

echo "verify: PASS"
