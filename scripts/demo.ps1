$ErrorActionPreference = "Stop"

Write-Host "== Go environment =="
go version

Write-Host "== Unit and integration tests =="
go test ./...

Write-Host "== Vet =="
go vet ./...

Write-Host "== Generate deterministic samples =="
go run ./cmd/verifytool generate -out data/samples -seed 20261124

Write-Host "== Acceptance demo =="
go run ./cmd/verifytool demo -data data/samples -out artifacts/demo-results.json

Write-Host ""
Write-Host "Demo result: artifacts/demo-results.json"
Write-Host "Start workbench with:"
Write-Host "  go run ./cmd/verifytool serve -data data/samples -addr 127.0.0.1:8090"
