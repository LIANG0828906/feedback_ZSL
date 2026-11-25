$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

go build ./...
go test ./...
go run ./cmd/qkernel demo --out demo-output --frames 3 --workers 4
go run ./cmd/qkernel verify --root demo-output | Out-File -Encoding utf8 verify-output.json

$report = Get-Content verify-output.json -Raw | ConvertFrom-Json
if (-not $report.ok) { throw "independent verification failed" }
Write-Host "all validation checks passed"
