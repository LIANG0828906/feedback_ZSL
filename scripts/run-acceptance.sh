#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SEED="${SEED:-1337}"
BASELINE="$ROOT/artifacts/baseline"
ACCEPTANCE="$ROOT/artifacts/acceptance"

cd "$ROOT"
dotnet build MeshRepair.sln -v:minimal
dotnet test MeshRepair.sln -v:minimal --no-build

if [[ -d "$BASELINE" ]]; then
  find "$BASELINE" -depth -delete
fi
if [[ -d "$ACCEPTANCE" ]]; then
  find "$ACCEPTANCE" -depth -delete
fi

dotnet run --project src/MeshRepair.Cli --no-build -- sample \
  --out "$BASELINE" \
  --seed "$SEED"

dotnet run --project src/MeshRepair.Cli --no-build -- verify \
  --out "$ACCEPTANCE" \
  --seed "$SEED"

dotnet run --project src/MeshRepair.Preview --no-build -- \
  --snapshots "$ACCEPTANCE/snapshots" \
  --headless \
  --out "$ACCEPTANCE/preview.ppm"

dotnet run --project src/MeshRepair.Cli --no-build -- recover \
  --snapshots "$ACCEPTANCE/snapshots"

sha256sum \
  "$ACCEPTANCE/trace.jsonl" \
  "$ACCEPTANCE/versions.json" \
  "$ACCEPTANCE/summary.json" \
  "$ACCEPTANCE/determinism.json" \
  "$ACCEPTANCE/preview.ppm"
