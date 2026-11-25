# Run from the repository root in Windows PowerShell.
$ErrorActionPreference = "Stop"

go env -w GOFLAGS=-mod=mod
$env:GOPROXY = "off"

go test ./...
go run ./cmd/macrotool generate
go run ./cmd/macrotool verify
go run ./cmd/macrotool inject -case all
