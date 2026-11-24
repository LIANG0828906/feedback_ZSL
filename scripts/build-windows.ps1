$ErrorActionPreference = "Stop"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw "Go 1.22+ is required."
}
if (-not (Get-Command wails -ErrorAction SilentlyContinue)) {
  throw "Install Wails v2: go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2"
}

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

go test ./...
go run ./cmd/strata generate -seed 195 -out data/samples
go run ./cmd/strata validate -dir data/samples -json artifacts/validation.json
wails build -s -skipbindings

Write-Host ""
Write-Host "Desktop build: $repo\build\bin\strata.exe"

