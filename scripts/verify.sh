#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
go env -w GOFLAGS=-mod=mod
GOPROXY=off go test ./...
GOPROXY=off go run ./cmd/macrotool generate
GOPROXY=off go run ./cmd/macrotool verify
GOPROXY=off go run ./cmd/macrotool inject -case all
