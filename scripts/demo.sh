#!/usr/bin/env bash
source "$(dirname "$0")/common.sh"

./scripts/generate.sh data
go run ./cmd/demo all --data data --artifacts artifacts/demo "$@"
echo
echo "first 12 scheduling records:"
head -n 12 artifacts/demo/schedule.jsonl
