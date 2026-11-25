$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
go run ./cmd/uvtool gen -out samples -seed 20261125
go run ./cmd/uvtool inspect -sample samples/sample_01 -truth | Out-Null
go run ./cmd/uvtool inspect -sample samples/sample_02 -truth | Out-Null
Write-Host "samples regenerated and truth-validated"
