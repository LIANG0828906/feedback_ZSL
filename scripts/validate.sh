#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
RUN="$ROOT/.validation"
BIN="$ROOT/bin/spectralforge"

mkdir -p "$ROOT/bin"
rm -rf "$RUN"
mkdir -p "$RUN"

go build -o "$BIN" ./cmd/spectralforge
go test ./...

"$BIN" sample --dir "$RUN/sample" --seed 20261125
"$BIN" validate --sample "$RUN/sample"

"$BIN" render --sample "$RUN/sample" --checkpoints "$RUN/cp-full" --out "$RUN/out-full" --mode full > "$RUN/full.json"
"$BIN" render --sample "$RUN/sample" --checkpoints "$RUN/cp-realtime" --out "$RUN/out-realtime" --mode realtime > "$RUN/realtime.json"
"$BIN" render --sample "$RUN/sample" --checkpoints "$RUN/cp-resume" --out "$RUN/out-resume" --mode realtime --stop-after 5 > "$RUN/pause.json"
"$BIN" render --sample "$RUN/sample" --checkpoints "$RUN/cp-resume" --out "$RUN/out-resume" --mode resume > "$RUN/resume.json"

"$BIN" verify --sample "$RUN/sample" --checkpoints "$RUN/cp-full" --out "$RUN/out-full" > "$RUN/verify.json"
"$BIN" compare --a "$RUN/out-full" --b "$RUN/out-realtime" --a-checkpoints "$RUN/cp-full" --b-checkpoints "$RUN/cp-realtime" > "$RUN/compare-realtime.json"
"$BIN" compare --a "$RUN/out-full" --b "$RUN/out-resume" --a-checkpoints "$RUN/cp-full" --b-checkpoints "$RUN/cp-resume" > "$RUN/compare-resume.json"

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$RUN/out-full/frame.png" "$RUN/out-realtime/frame.png" "$RUN/out-resume/frame.png"
else
  shasum -a 256 "$RUN/out-full/frame.png" "$RUN/out-realtime/frame.png" "$RUN/out-resume/frame.png"
fi
echo "Validation artifacts: $RUN"
