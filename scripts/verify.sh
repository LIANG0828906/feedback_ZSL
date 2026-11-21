#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

if ! ldconfig -p 2>/dev/null | grep -q 'libicuuc'; then
  export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1
fi

dotnet build BattleEconomy.Replay.sln
dotnet run --project tests/BattleEconomy.Verify/BattleEconomy.Verify.csproj --no-build
