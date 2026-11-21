#!/usr/bin/env bash
set -euo pipefail

binary="${1:?font_atlas binary path is required}"
work="${2:?work directory is required}"

cmake -E remove_directory "$work"
cmake -E make_directory "$work"

"$binary" generate --out "$work/samples_a" --seed 777 >/dev/null
"$binary" generate --out "$work/samples_b" --seed 777 >/dev/null
diff -rq "$work/samples_a" "$work/samples_b"

"$binary" verify --samples "$work/samples_a" --out "$work/verify"

"$binary" raster --font "$work/samples_a/font.json" \
    --config "$work/samples_a/config.json" --out "$work/full" >/dev/null
cp "$work/full/result.json" "$work/full_result_1.json"
cp "$work/full/trace.jsonl" "$work/full_trace_1.jsonl"
cp "$work/full/atlas.pgm" "$work/full_atlas_1.pgm"
cp "$work/full/atlas.json" "$work/full_atlas_1.json"
"$binary" raster --font "$work/samples_a/font.json" \
    --config "$work/samples_a/config.json" --out "$work/full" >/dev/null
cmp "$work/full_result_1.json" "$work/full/result.json"
cmp "$work/full_trace_1.jsonl" "$work/full/trace.jsonl"
cmp "$work/full_atlas_1.pgm" "$work/full/atlas.pgm"
cmp "$work/full_atlas_1.json" "$work/full/atlas.json"

set +e
"$binary" raster --font "$work/samples_a/font.json" \
    --config "$work/samples_a/config.json" --out "$work/crash" \
    --state "$work/crash/state" --hard-exit-after 2 >/dev/null 2>&1
crash_status=$?
set -e
test "$crash_status" -eq 77
test ! -e "$work/crash/atlas.pgm"

"$binary" raster --font "$work/samples_a/font.json" \
    --config "$work/samples_a/config.json" --out "$work/crash" \
    --state "$work/crash/state" --resume >/dev/null
cmp "$work/full/atlas.pgm" "$work/crash/atlas.pgm"
cmp "$work/full/atlas.json" "$work/crash/atlas.json"

python3 - "$work/crash/result.json" <<'PY'
import json
import sys

result = json.load(open(sys.argv[1], encoding="utf-8"))
assert result["ok"] is True
assert any(g["reusedChunks"] > 0 for g in result["glyphs"]), result["glyphs"]
assert result["atlas"]["imageHash"]
PY

"$binary" compare-quant --font "$work/samples_a/font.json" \
    --config "$work/samples_a/config.json" --old 1 --new 8 \
    --out "$work/quant.json"
python3 - "$work/quant.json" <<'PY'
import json
import sys

summary = json.load(open(sys.argv[1], encoding="utf-8"))
assert summary["totalAbsoluteError"] > 0
assert summary["glyphMaxAbsoluteError"] > 0
assert summary["glyphs"]
for glyph in summary["glyphs"]:
    assert glyph["glyphMaxAbsoluteError"] >= 0
PY

echo "end-to-end determinism, recovery, and quantization checks passed"

