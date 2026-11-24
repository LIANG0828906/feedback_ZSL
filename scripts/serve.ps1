$ErrorActionPreference = "Stop"

if (-not (Test-Path "data/samples/catalog.json")) {
    go run ./cmd/verifytool generate -out data/samples -seed 20261124
}

go run ./cmd/verifytool serve -data data/samples -addr 127.0.0.1:8090
