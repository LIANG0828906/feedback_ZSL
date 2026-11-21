#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source scripts/env.sh

OUT="$ROOT/artifacts/generated/verification"
rm -rf "$OUT"

"$LA_PYTHON" -m loudness_archive generate --out "$OUT" --seed 20261122 --summary
"$LA_PYTHON" -m unittest discover -v

echo
echo "Deterministic artifacts:"
sha256sum \
  "$OUT/baseline.json" \
  "$OUT/review-report.json" \
  "$OUT/trace.jsonl"

