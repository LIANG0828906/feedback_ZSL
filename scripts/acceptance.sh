#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=${1:-"$ROOT/work"}
SEED=${SEED:-42}

cd "$ROOT"
case "$WORK" in
  ""|"/") echo "refusing unsafe work directory: $WORK" >&2; exit 2 ;;
esac
rm -rf "$WORK/state" "$WORK/checkpoints" "$WORK/cache" "$WORK/exports" "$WORK/reports"
mkdir -p "$WORK/reports"

go test ./...
(cd desktop && go test ./...)
go run ./cmd/vspipeline sample --out "$WORK" --seed "$SEED" >"$WORK/reports/sample.json"
go run ./cmd/vspipeline validate --work "$WORK" >"$WORK/reports/validate.json"
go run ./cmd/vspipeline full --work "$WORK" >"$WORK/reports/full.json"
go run ./cmd/vspipeline incremental --work "$WORK" \
  --kind layer_parameter --layer prop --opacity 0.61 >"$WORK/reports/incremental.json"
go run ./cmd/vspipeline verify-incremental --work "$WORK" \
  --layer prop --opacity 0.57 >"$WORK/reports/verify-incremental.json"
go run ./cmd/vspipeline export --work "$WORK" --version 1 >"$WORK/reports/export-v1.json"
go run ./cmd/vspipeline export --work "$WORK" --version 2 >"$WORK/reports/export-v2.json"
go run ./cmd/vspipeline export --work "$WORK" --version 3 >"$WORK/reports/export-v3.json"
go run ./cmd/vspipeline resume --work "$WORK" >"$WORK/reports/resume.json"

CORRUPT=$(mktemp -d "${TMPDIR:-/tmp}/vsp-corrupt-XXXXXX")
trap 'rm -rf "$CORRUPT"' EXIT
cp -a "$WORK/." "$CORRUPT/"
printf 'tampered' >>"$CORRUPT/cache/frames/window-00002.vspw"
go run ./cmd/vspipeline resume --work "$CORRUPT" >"$WORK/reports/corrupt-resume.json"

cat <<EOF
Acceptance completed.
  work: $WORK
  validation: $WORK/reports/validate.json
  incremental verification: $WORK/reports/verify-incremental.json
  exports: $WORK/reports/export-v1.json $WORK/reports/export-v2.json $WORK/reports/export-v3.json
  recovery: $WORK/reports/resume.json
  corrupt recovery: $WORK/reports/corrupt-resume.json
EOF
