$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
go run ./cmd/strata generate -seed 195 -out data/samples
go run -tags web . -addr 127.0.0.1:4173

