# IconBench local Windows demo. Requires Go 1.22+.
$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "0"

Write-Host "[1/4] go version" -ForegroundColor Cyan
go version

Write-Host "[2/4] download pure-Go SQLite dependency" -ForegroundColor Cyan
go mod download

Write-Host "[3/4] deterministic samples + full recompute + independent verification" -ForegroundColor Cyan
go run ./cmd/workbench generate -data data
go run ./cmd/workbench recompute -data data
go run ./cmd/verify -data data

Write-Host "[4/4] starting http://127.0.0.1:8080" -ForegroundColor Green
Start-Process "http://127.0.0.1:8080"
go run ./cmd/workbench -addr 127.0.0.1:8080 -data data
