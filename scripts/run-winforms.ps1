$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
dotnet run --project src/BattleEconomy.WinForms/BattleEconomy.WinForms.csproj
