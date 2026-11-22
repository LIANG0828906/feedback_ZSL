#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

echo "== .NET SDK =="
dotnet --version

echo "== Generate deterministic sample =="
dotnet run --project src/GameEditor.Tools/GameEditor.Tools.csproj -- generate

echo "== Build solution =="
dotnet build GameEditor.sln -c Release --nologo

echo "== Acceptance suite =="
dotnet run --project tests/GameEditor.Tests/GameEditor.Tests.csproj -c Release --no-build

echo "== Console demo and snapshot round trip =="
dotnet run --project src/GameEditor.Tools/GameEditor.Tools.csproj -c Release --no-build -- demo

echo "Verification completed."
