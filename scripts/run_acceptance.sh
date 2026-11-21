#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD="${BUILD_DIR:-$ROOT/build}"
OUT="${1:-$ROOT/artifacts/acceptance}"
EXE="$BUILD/chunkpipe"

cmake -S "$ROOT" -B "$BUILD" -DCMAKE_BUILD_TYPE=Release
cmake --build "$BUILD" --config Release -j 4
ctest --test-dir "$BUILD" -C Release --output-on-failure

cmake -E remove_directory "$OUT"
mkdir -p "$OUT"

"$EXE" generate --out "$OUT/samples"
"$EXE" run --archive "$OUT/samples/scene_v1.arc" --out "$OUT/run-v1" \
    --workers 3 --delay-us 150 >"$OUT/run-v1.log"
"$EXE" run --archive "$OUT/samples/scene_v2.arc" --out "$OUT/run-v2" \
    --workers 3 --delay-us 150 >"$OUT/run-v2.log"
"$EXE" run --archive "$OUT/samples/scene_v3.arc" --out "$OUT/run-v3" \
    --workers 3 --delay-us 150 >"$OUT/run-v3.log"
"$EXE" compare --dir "$OUT/samples" --out "$OUT/compare" >"$OUT/compare.log"
"$EXE" compare --dir "$OUT/samples" --out "$OUT/compare-partial" \
    --max-loaded 3 >"$OUT/compare-partial.log"
"$EXE" determinism --archive "$OUT/samples/scene_v3.arc" \
    --out "$OUT/determinism" >"$OUT/determinism.log"

set +e
"$EXE" export --archive "$OUT/samples/scene_v3.arc" \
    --out "$OUT/scene_v3_to_v2.arc" --to-version 2 >"$OUT/export-loss.log" 2>&1
EXPORT_STATUS=$?
set -e
if [[ "$EXPORT_STATUS" -ne 3 ]]; then
    echo "expected E_VERSION_LOSS exit status 3, got $EXPORT_STATUS" >&2
    exit 1
fi

set +e
"$EXE" run --archive "$OUT/samples/scene_v3.arc" --out "$OUT/crash" \
    --workers 3 --delay-us 100 --crash-after 3 >"$OUT/crash-first.log" 2>&1
CRASH_STATUS=$?
set -e
if [[ "$CRASH_STATUS" -ne 70 ]]; then
    echo "expected forced termination status 70, got $CRASH_STATUS" >&2
    exit 1
fi
COMMITTED_BEFORE="$(find "$OUT/crash/recovery" -name '*.snap' | wc -l)"
if [[ "$COMMITTED_BEFORE" -ne 3 ]]; then
    echo "expected 3 committed recovery chunks, got $COMMITTED_BEFORE" >&2
    exit 1
fi

"$EXE" recover --archive "$OUT/samples/scene_v3.arc" --out "$OUT/crash" \
    --workers 3 --delay-us 100 >"$OUT/crash-recover.log"
COMMITTED_AFTER="$(find "$OUT/crash/recovery" -name '*.snap' | wc -l)"
if [[ "$COMMITTED_AFTER" -ne 9 ]]; then
    echo "expected 9 committed recovery chunks, got $COMMITTED_AFTER" >&2
    exit 1
fi

grep -q '"deterministic":true' "$OUT/determinism/determinism.json"
grep -q 'format_degradation' "$OUT/compare/diff.jsonl"
grep -q 'load_order' "$OUT/compare-partial/diff.jsonl"
grep -q 'priority_derivation' "$OUT/compare/diff_report.json"
grep -q '"code":"E_VERSION_LOSS"' "$OUT/error.json"

NORMAL_HASH="$(sed -n 's/.*content_hash=\([0-9a-f]*\).*/\1/p' "$OUT/run-v3.log" | head -1)"
RECOVERED_HASH="$(sed -n 's/.*content_hash=\([0-9a-f]*\).*/\1/p' "$OUT/crash-recover.log" | head -1)"
if [[ -z "$NORMAL_HASH" || "$NORMAL_HASH" != "$RECOVERED_HASH" ]]; then
    echo "normal and recovered v3 snapshot hashes differ" >&2
    exit 1
fi

echo "acceptance: PASS"
echo "output: $OUT"
echo "v1 mask: $(cat "$OUT/run-v1/visibility.json" | sed -n 's/.*"mask":"\([0-9a-f]*\)".*/\1/p')"
echo "v2 mask: $(cat "$OUT/run-v2/visibility.json" | sed -n 's/.*"mask":"\([0-9a-f]*\)".*/\1/p')"
echo "v3 mask: $(cat "$OUT/run-v3/visibility.json" | sed -n 's/.*"mask":"\([0-9a-f]*\)".*/\1/p')"
echo "v3 snapshot hash: $NORMAL_HASH"
echo "recovery commits: $COMMITTED_BEFORE -> $COMMITTED_AFTER"

