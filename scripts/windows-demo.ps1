$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
$env:CGO_ENABLED = "0"

if (-not (Test-Path "samples/manifest.json")) {
    go run ./cmd/vcomp gen --out samples --seed 42
}

go run ./cmd/vcomp workbench --data samples --version v3 --db artifacts/workbench.db `
    --project workbench --demo --out artifacts/workbench

