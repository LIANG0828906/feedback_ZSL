#!/usr/bin/env bash

FK_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOMODCACHE="$FK_ROOT/.gocache/mod"
export GOCACHE="$FK_ROOT/.gocache/build"
export GOPROXY="${GOPROXY:-off}"
export GO111MODULE=on
