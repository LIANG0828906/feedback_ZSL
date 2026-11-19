#!/usr/bin/env bash
# End-to-end validation entry point: reuse/start SQL Server, build the .NET 8
# gateway and execute every scenario, leaving reports under artifacts/run.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=env.sh
source "$ROOT_DIR/scripts/env.sh"

export OG_DATABASE="${OG_DATABASE:-OrdersDb}"
export OG_MAX_CONNECTIONS="${OG_MAX_CONNECTIONS:-6}"
export OG_ARTIFACT_DIR="${OG_ARTIFACT_DIR:-$ROOT_DIR/artifacts/run}"
export DOTNET_PROCESSOR_COUNT="${DOTNET_PROCESSOR_COUNT:-2}"

if [ -z "${OG_CONNECTION_STRING:-}" ] && [ -z "${OG_SA_PASSWORD:-}" ]; then
  echo "[env] export OG_SA_PASSWORD or OG_CONNECTION_STRING first" >&2
  exit 1
fi

bash "$ROOT_DIR/scripts/start-sqlserver.sh"

if [ ! -x "$DOTNET_HOME/dotnet" ]; then
  echo "[build] .NET 8 SDK missing at $DOTNET_HOME; run scripts/install-dotnet.sh first" >&2
  exit 1
fi

mkdir -p "$OG_ARTIFACT_DIR"
cd "$ROOT_DIR/src/OrderGateway"
"$DOTNET_HOME/dotnet" build -c Release -m:2

DOTNET_GCHeapHardLimit="${DOTNET_GCHeapHardLimit:-268435456}" \
DOTNET_PROCESSOR_COUNT=2 \
  "$DOTNET_HOME/dotnet" run -c Release --no-build
