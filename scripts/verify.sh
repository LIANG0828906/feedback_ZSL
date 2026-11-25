#!/usr/bin/env bash
source "$(dirname "$0")/common.sh"

data="${1:-data}"
artifacts="${2:-artifacts/demo}"
go run ./cmd/verify -data "$data" -artifacts "$artifacts"
