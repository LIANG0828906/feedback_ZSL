#!/usr/bin/env bash
source "$(dirname "$0")/common.sh"

out="${1:-data}"
go run ./cmd/generate -out "$out"
