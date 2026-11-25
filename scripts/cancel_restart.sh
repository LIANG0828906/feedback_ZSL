#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="work/cancel-restart"
rm -rf -- "$WORK"
mkdir -p bin "$WORK"
go build -o bin/uvtool ./cmd/uvtool

DB="$WORK/cache.db"
RECOVERY_OUT="$WORK/recovery"
FULL_OUT="$WORK/full"

# Establish the baseline snapshot.
bin/uvtool run \
  -sample samples/sample_01 \
  -params configs/default_params.json \
  -db "$DB" \
  -out "$WORK/initial" >/dev/null

# Persist exactly one newly committed stage, then return E_CANCELLED / exit 7.
set +e
bin/uvtool run \
  -sample samples/sample_01 \
  -params configs/edit_u_gain.json \
  -db "$DB" \
  -out "$RECOVERY_OUT" \
  -cancel-after-stages 1 >"$WORK/cancel.out" 2>"$WORK/cancel.err"
status=$?
set -e
if [[ "$status" -ne 7 ]]; then
  echo "expected graceful cancel exit 7, got $status" >&2
  cat "$WORK/cancel.err" >&2
  exit 1
fi
if ! grep -q '"complete": false' "$RECOVERY_OUT/partial_run.json"; then
  echo "canceled run incorrectly claimed a complete report" >&2
  exit 1
fi

# Restart from the matching partial snapshot and finish the remaining stage.
bin/uvtool run \
  -sample samples/sample_01 \
  -params configs/edit_u_gain.json \
  -db "$DB" \
  -out "$RECOVERY_OUT" >/dev/null

# One-shot full recomputation from an empty cache for comparison.
bin/uvtool run \
  -sample samples/sample_01 \
  -params configs/edit_u_gain.json \
  -db "$WORK/full.db" \
  -out "$FULL_OUT" >/dev/null

cmp "$RECOVERY_OUT/enhanced.yuv444p" "$FULL_OUT/enhanced.yuv444p"
cmp "$RECOVERY_OUT/difference.pgm" "$FULL_OUT/difference.pgm"
echo "cancel persisted a partial snapshot; restart matched full recomputation byte-for-byte"
