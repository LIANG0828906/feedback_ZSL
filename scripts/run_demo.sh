#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ -n "${PYTHON:-}" ]]; then
  :
elif [[ -x "$ROOT/.venv/bin/python" ]]; then
  PYTHON="$ROOT/.venv/bin/python"
else
  PYTHON="python3"
fi
"$PYTHON" -c "import numpy" >/dev/null

OUT="${1:-artifacts/demo}"
rm -rf "$OUT"
mkdir -p "$OUT"

run_suite() {
  local target="$1"
  "$PYTHON" -m docagg run \
    --input samples/events.jsonl \
    --pipeline samples/pipeline.json \
    --reference \
    --reference-output "$target/reference.json" \
    --output "$target/normal_report.json" \
    --error-output "$target/normal_errors.json" \
    --pretty 2>"$target/normal_errors.stderr.jsonl"

  "$PYTHON" -m docagg run \
    --input samples/anomalies.jsonl \
    --pipeline samples/pipeline.json \
    --reference \
    --output "$target/anomalies_report.json" \
    --error-output "$target/anomalies_errors.json" \
    --pretty 2>"$target/anomalies_errors.stderr.jsonl"

  "$PYTHON" -m docagg run \
    --input samples/events.jsonl \
    --pipeline samples/pipeline.json \
    --batch-size 3 \
    --max-active-groups 1000 \
    --max-state-bytes 100000000 \
    --reference-file "$target/reference.json" \
    --output "$target/no_spill_report.json" \
    --pretty 2>"$target/no_spill_errors.stderr.jsonl"

  "$PYTHON" -m docagg run \
    --input samples/events.jsonl \
    --pipeline samples/pipeline.json \
    --batch-size 4 \
    --max-active-groups 1 \
    --fail-spill-on-attempt 1 \
    --reference-file "$target/reference.json" \
    --output "$target/spill_failure_report.json" \
    --pretty 2>"$target/spill_failure_errors.stderr.jsonl"
}

run_suite "$OUT"

REPEAT_DIR="$(mktemp -d)"
trap 'rm -rf "$REPEAT_DIR"' EXIT
run_suite "$REPEAT_DIR"

for name in \
  normal_report.json \
  reference.json \
  anomalies_report.json \
  no_spill_report.json \
  spill_failure_report.json
do
  cmp "$OUT/$name" "$REPEAT_DIR/$name"
done

"$PYTHON" scripts/summarize_demo.py \
  --artifacts "$OUT" \
  --repeat-artifacts "$REPEAT_DIR" \
  --output "$OUT/summary.json"
