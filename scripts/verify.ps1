$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

go version
go test ./...
go run ./cmd/terrainpatch generate -dir samples/local -seed 42
go run ./cmd/terrainpatch verify -samples samples/local -db data/verify.db
Write-Host "Verification passed. Open http://localhost:8080 after scripts/serve.ps1"
