$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

if ($env:CGO_ENABLED -ne "1") {
    $env:CGO_ENABLED = "1"
}

go test ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go run ./cmd/verify -samples samples -seed 20261124
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
