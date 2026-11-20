#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="${1:-$ROOT/build}"
WORK_DIR="${2:-$ROOT/regression-out}"
BIN="$BUILD_DIR/fill_pipeline"

if [[ ! -x "$BIN" ]]; then
    cmake -S "$ROOT" -B "$BUILD_DIR" -DCMAKE_BUILD_TYPE=Release
    cmake --build "$BUILD_DIR" --parallel
fi

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

echo "[1/6] generate deterministic samples"
"$BIN" generate-samples --out "$WORK_DIR/samples" --width 64 --height 64 \
    >"$WORK_DIR/generate.stdout.json"

echo "[2/6] render each sample twice"
mapfile -t SAMPLE_FILES < <(
    find "$WORK_DIR/samples" -maxdepth 1 -name '*.json' \
        ! -name 'manifest.json' -printf '%f\n' | sort
)
for sample in "${SAMPLE_FILES[@]}"; do
    sample_name="${sample%.json}"
    "$BIN" render --input "$WORK_DIR/samples/$sample" \
        --out "$WORK_DIR/runs/$sample_name/a" >/dev/null
    "$BIN" render --input "$WORK_DIR/samples/$sample" \
        --out "$WORK_DIR/runs/$sample_name/b" >/dev/null
done

echo "[3/6] compare images and trace logs byte-for-byte"
for sample in "${SAMPLE_FILES[@]}"; do
    sample_name="${sample%.json}"
    diff -r "$WORK_DIR/runs/$sample_name/a" "$WORK_DIR/runs/$sample_name/b" \
        >/dev/null
done

echo "[4/6] compare coverage images with generated baseline hashes"
python3 "$ROOT/scripts/verify_baselines.py" \
    "$WORK_DIR/samples/manifest.json" "$WORK_DIR/runs"

echo "[5/6] verify parse-entry injection rejection"
for kind in overlap-edge degenerate-segment zero-area hole-direction nonfinite; do
    output_dir="$WORK_DIR/injections/$kind"
    set +e
    response="$("$BIN" render --input "$WORK_DIR/samples/nested_hole.json" \
        --out "$output_dir" --inject "$kind")"
    status=$?
    set -e
    if [[ $status -ne 2 ]]; then
        echo "injection $kind returned $status instead of 2" >&2
        exit 1
    fi
    if [[ -e "$output_dir" ]]; then
        echo "injection $kind wrote output before rejection" >&2
        exit 1
    fi
    if [[ "$response" != *'"injected":true'* ]]; then
        echo "injection $kind has no injected marker: $response" >&2
        exit 1
    fi
done
set +e
budget_response="$("$BIN" render --input "$WORK_DIR/samples/nested_hole.json" \
    --out "$WORK_DIR/injections/invalid-budget" \
    --budget 0,0.001,0.01,2)"
budget_status=$?
set -e
if [[ $budget_status -ne 2 || "$budget_response" != *'E_TOLERANCE_INVALID'* ]]; then
    echo "invalid tolerance budget was not rejected" >&2
    exit 1
fi
if [[ -e "$WORK_DIR/injections/invalid-budget" ]]; then
    echo "invalid tolerance budget wrote output before rejection" >&2
    exit 1
fi

echo "[6/6] verify cancellation, degradation and submission gates"
"$BIN" render --input "$WORK_DIR/samples/nested_hole.json" \
    --out "$WORK_DIR/cancel" >/dev/null
before_hash="$(sha256sum "$WORK_DIR/cancel/fill_bitmap.pgm" | cut -d' ' -f1)"
set +e
cancel_response="$("$BIN" render --input "$WORK_DIR/samples/nested_hole.json" \
    --out "$WORK_DIR/cancel" --cancel-after-blocks 1)"
cancel_status=$?
set -e
if [[ $cancel_status -ne 4 || "$cancel_response" != *'E_CANCELLED'* ]]; then
    echo "cancellation did not return E_CANCELLED" >&2
    exit 1
fi
after_hash="$(sha256sum "$WORK_DIR/cancel/fill_bitmap.pgm" | cut -d' ' -f1)"
if [[ "$before_hash" != "$after_hash" ]]; then
    echo "cancelled render replaced the published bitmap" >&2
    exit 1
fi

"$BIN" render --input "$WORK_DIR/samples/nested_hole.json" \
    --out "$WORK_DIR/submit-ok" >/dev/null
"$BIN" submit --result "$WORK_DIR/submit-ok/result.json" >/dev/null

"$BIN" render --input "$WORK_DIR/samples/nested_same_direction.json" \
    --out "$WORK_DIR/submit-degraded" >/dev/null
set +e
degraded_response="$("$BIN" submit \
    --result "$WORK_DIR/submit-degraded/result.json")"
degraded_status=$?
set -e
if [[ $degraded_status -ne 9 || "$degraded_response" != *'E_DEGRADED_RESULT'* ]]; then
    echo "degraded submission was not rejected" >&2
    exit 1
fi

printf 'regression ok: %d samples, baseline hashes match, outputs deterministic, cancellation preserved bitmap\n' \
    "${#SAMPLE_FILES[@]}"
