#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT="${FORENSICLAB_ARTIFACTS:-artifacts}"
mkdir -p "$OUT/samples" "$OUT/acceptance" "$OUT/reports" "$OUT/acceptance/threeway"
find "$OUT/acceptance" -maxdepth 1 -type f \( -name '*.sqlite' -o -name '*.sqlite-wal' -o -name '*.sqlite-shm' \) -delete
find "$OUT/acceptance/threeway" -mindepth 1 -delete

echo "== unit, integration and race tests =="
go test ./... -count=1
go test -race ./internal/forensic -count=1

echo "== deterministic samples =="
go run ./cmd/forensiclab generate --dir "$OUT/samples" > "$OUT/acceptance/generate.json"

echo "== incremental threshold edits =="
go run ./cmd/forensiclab sequence \
  --manifest "$OUT/samples/manifest.json" \
  --image game_tex_rgb_160x112 \
  --db "$OUT/acceptance/incremental.sqlite" \
  --thresholds 0.70,0.45,0.30 \
  --report "$OUT/reports" > "$OUT/acceptance/sequence.json"

echo "== three-path pixel/block/sequence verification =="
go run ./cmd/forensiclab verify \
  --manifest "$OUT/samples/manifest.json" \
  --image game_tex_rgb_160x112 \
  --workdir "$OUT/acceptance/threeway" \
  --thresholds 0.70,0.45 \
  --report "$OUT/reports" > "$OUT/acceptance/threeway.json"
grep -q '"equal": true' "$OUT/acceptance/threeway.json"

echo "== abrupt termination and durable resume =="
go build -o "$OUT/acceptance/forensiclab" ./cmd/forensiclab
set +e
"$OUT/acceptance/forensiclab" run \
  --manifest "$OUT/samples/manifest.json" \
  --image game_tex_rgb_160x112 \
  --db "$OUT/acceptance/kill.sqlite" \
  --threshold 0.45 --block 16 --window 3 --kill-after 5 \
  > "$OUT/acceptance/kill-run.json" 2> "$OUT/acceptance/kill-run.err"
KILL_CODE=$?
set -e
if [ "$KILL_CODE" -ne 99 ]; then
  echo "expected forced termination exit code 99, got $KILL_CODE" >&2
  exit 1
fi
echo "$KILL_CODE" > "$OUT/acceptance/kill-exit-code.txt"

"$OUT/acceptance/forensiclab" run \
  --manifest "$OUT/samples/manifest.json" \
  --image game_tex_rgb_160x112 \
  --db "$OUT/acceptance/kill.sqlite" \
  --threshold 0.45 --block 16 --window 3 \
  > "$OUT/acceptance/resume-run.json"
grep -q '"reusedBlocks": 5' "$OUT/acceptance/resume-run.json"

"$OUT/acceptance/forensiclab" run \
  --manifest "$OUT/samples/manifest.json" \
  --image game_tex_rgb_160x112 \
  --db "$OUT/acceptance/kill.sqlite" \
  --threshold 0.45 --block 16 --window 5 \
  > "$OUT/acceptance/version-mismatch-run.json"
grep -q '"snapshotResetReason": "params_version_mismatch"' "$OUT/acceptance/version-mismatch-run.json"

"$OUT/acceptance/forensiclab" summarize --dir "$OUT/acceptance"

echo "Acceptance artifacts written to $OUT/acceptance"
