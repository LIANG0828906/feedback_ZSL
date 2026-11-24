$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
$env:CGO_ENABLED = "0"

go version
go test ./...
go build -trimpath -o artifacts/windows/vcomp.exe ./cmd/vcomp

if (-not (Test-Path "samples/manifest.json")) {
    go run ./cmd/vcomp gen --out samples --seed 42
}

go run ./cmd/vcomp verify --data samples --work artifacts/windows/verify
go run ./cmd/vcomp workbench --data samples --version v3 --db artifacts/windows/workbench.db `
    --project workbench --demo --out artifacts/windows/workbench

Write-Host "Windows build, tests, verification and demo completed."
Write-Host "Persistent database: artifacts/windows/workbench.db"

