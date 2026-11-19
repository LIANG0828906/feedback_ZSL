#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export DOTNET_CLI_TELEMETRY_OPTOUT=1
exec dotnet run --project "$ROOT/src/InventoryMigration.Api/InventoryMigration.Api.csproj" \
  --urls "${ASPNETCORE_URLS:-http://127.0.0.1:5080}"
