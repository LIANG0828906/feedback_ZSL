#!/usr/bin/env sh
set -eu

go version
go test ./...
go vet ./...
go run ./cmd/verifytool generate -out data/samples -seed 20261124
go run ./cmd/verifytool demo -data data/samples -out artifacts/demo-results.json
