#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BUILD_DIR=${BUILD_DIR:-"$ROOT/build"}
OUT_DIR=${1:-"$ROOT/out/samples"}
SEED=${SEED:-20261121}
CASES=${CASES:-3}

cmake -S "$ROOT" -B "$BUILD_DIR" -DCMAKE_BUILD_TYPE=Release
cmake --build "$BUILD_DIR" -j
"$BUILD_DIR/battle_cli" generate --out "$OUT_DIR" --seed "$SEED" --cases "$CASES"

