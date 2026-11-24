param(
    [string]$Dir = "artifacts/acceptance"
)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$env:CGO_ENABLED = "0"
if (-not $env:GOFLAGS) { $env:GOFLAGS = "-mod=vendor" }

go version
go env GOMODCACHE
& "$PSScriptRoot/Generate-Sample.ps1"
go test ./...
go run ./cmd/terrain-acceptance -dir $Dir
Write-Host "DONE. See $Dir/acceptance-report.json"
