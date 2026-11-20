#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BUILD_DIR=${BUILD_DIR:-"$ROOT/build"}
SAMPLES="$ROOT/out/validation-samples"
RUN_DIR="$ROOT/out/validation-run"

cmake -S "$ROOT" -B "$BUILD_DIR" -DCMAKE_BUILD_TYPE=Release
cmake --build "$BUILD_DIR" -j
cmake --build "$BUILD_DIR" --target test

"$BUILD_DIR/battle_cli" generate --out "$SAMPLES" --seed 20261121 --cases 3

for case_name in case-000 case-001 case-002; do
    "$BUILD_DIR/battle_cli" run \
        --scene "$SAMPLES/$case_name/scene.bin" \
        --ops "$SAMPLES/$case_name/ops.txt" \
        --trace "$RUN_DIR/$case_name/trace.log" \
        --snapshots "$RUN_DIR/$case_name/snapshots"
    cmp "$SAMPLES/$case_name/baseline/trace.log" \
        "$RUN_DIR/$case_name/trace.log"
    "$BUILD_DIR/battle_cli" replay \
        --trace "$RUN_DIR/$case_name/trace.log" \
        --snapshots "$RUN_DIR/$case_name/snapshots"
    sha256sum "$RUN_DIR/$case_name/trace.log"
done

echo "validation passed: all baseline traces are byte-identical"

