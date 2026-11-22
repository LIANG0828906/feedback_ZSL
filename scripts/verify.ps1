$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

Write-Host "== .NET SDK =="
dotnet --version

Write-Host "== Generate deterministic sample =="
dotnet run --project "src/GameEditor.Tools/GameEditor.Tools.csproj" -- generate

Write-Host "== Build solution (console, library, WinForms, tests) =="
dotnet build "GameEditor.sln" -c Release --nologo

Write-Host "== Acceptance suite =="
dotnet run --project "tests/GameEditor.Tests/GameEditor.Tests.csproj" -c Release --no-build

Write-Host "== Console demo and snapshot round trip =="
dotnet run --project "src/GameEditor.Tools/GameEditor.Tools.csproj" -c Release --no-build -- demo

Write-Host "Verification completed."
