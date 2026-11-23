#!/usr/bin/env bash
# Pure three-path comparison; never mutates any checkpoint.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

build_all
"${ROOT}/bin/compare" -a "${STORE_ROOT}/realtime" -b "${STORE_ROOT}/rebuild"
"${ROOT}/bin/compare" -a "${STORE_ROOT}/realtime" -b "${STORE_ROOT}/resume"
