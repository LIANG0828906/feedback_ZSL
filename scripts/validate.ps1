$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
go test ./...
go test -tags web .
go run ./cmd/strata generate -seed 195 -out data/samples
go run ./cmd/strata validate -dir data/samples -json artifacts/validation.json

