$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

if (-not (Test-Path "samples/default/scene.json")) {
    dotnet run --project "src/GameEditor.Tools/GameEditor.Tools.csproj" -- generate
}

dotnet run --project "src/GameEditor.WinForms/GameEditor.WinForms.csproj" -c Release
