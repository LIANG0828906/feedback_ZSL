param(
  [string]$Output = "texel-forge.exe",
  [int64]$Seed = 42
)

$ErrorActionPreference = "Stop"
go version
go test ./...
go run ./cmd/workbench sample --seed $Seed --out .\data
go run ./cmd/workbench preflight --data .\data --checkpoints .\data\checkpoints
go build -o $Output ./cmd/workbench

Write-Host ""
Write-Host "Built $Output"
Write-Host "Run:"
Write-Host "  .\$Output serve --data .\data --checkpoints .\data\checkpoints"
Write-Host "Open: http://127.0.0.1:8787"

