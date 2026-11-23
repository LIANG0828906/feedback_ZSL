#!/usr/bin/env sh
set -eu

go test ./...
go test -race ./...
go vet ./...

go run ./cmd/stagecue validate --project samples/small-42/project.json

go run ./cmd/stagecue verify \
  --project samples/small-42/project.json \
  --workers 2 \
  --buffer 250000 \
  --memory 300000 \
  --force-mode exact \
  --work-dir /tmp/stagecue-verify-exact

go run ./cmd/stagecue verify \
  --project samples/small-42/project.json \
  --workers 3 \
  --buffer 250000 \
  --memory 300000 \
  --force-mode merged \
  --work-dir /tmp/stagecue-verify-merged
