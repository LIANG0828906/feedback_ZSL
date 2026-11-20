#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1
export DOTNET_CLI_TELEMETRY_OPTOUT=1
export DOTNET_NOLOGO=1

mkdir -p artifacts/logs artifacts/verification

echo "== dotnet =="
dotnet --version

echo "== build (Release, includes WinForms cross-target) =="
dotnet build TacticalField.sln -c Release --nologo

echo "== verification suite =="
dotnet run --project tests/TacticalField.Verification/TacticalField.Verification.csproj -c Release --no-build

echo "== deterministic samples =="
dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  generate-samples --data-dir data --force
dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  verify-samples --data-dir data

for sample in small_8x8 medium_12x10 large_16x12; do
  dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
    run-sample "$sample" --data-dir data --out "artifacts/logs/${sample}.log" 2>&1
done

echo "== snapshot and forced-termination demo =="
DEMO_DIR="artifacts/verification/runtime-$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$DEMO_DIR"

set +e
dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  crash-demo --data-dir "$DEMO_DIR" >"$DEMO_DIR/crash.log" 2>&1
CRASH_CODE=$?
set -e
if [[ "$CRASH_CODE" -eq 0 ]]; then
  echo "ERROR crash-demo exited successfully" >&2
  exit 1
fi
echo "forced-termination exit_code=$CRASH_CODE"
tail -n 4 "$DEMO_DIR/crash.log"

dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  recover --data-dir "$DEMO_DIR" | tee "$DEMO_DIR/recover.log"

WINDOWS_DOTNET="${WINDOWS_DOTNET:-/mnt/c/Users/LIANG/AppData/Local/Microsoft/dotnet8-desktop/dotnet.exe}"
if [[ -x "$WINDOWS_DOTNET" ]]; then
  echo "== WinForms runtime smoke =="
  ./scripts/smoke-gui-windows.sh 2>&1 | tee artifacts/logs/gui-smoke.log
else
  echo "== WinForms runtime smoke: skipped (Windows Desktop runtime host not found) =="
fi

echo "== verify PASS =="

