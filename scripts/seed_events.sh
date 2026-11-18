#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COUNT="${1:-10000}"
DUPLICATE_EVERY="${2:-33}"
PARTITIONS="${TOPIC_PARTITIONS:-6}"

cd "$ROOT"
exec mvn -q -DskipTests compile exec:java \
  -Dexec.mainClass=com.eventbus.tool.SeedEventsMain \
  -Dexec.args="--count=$COUNT --duplicate-every=$DUPLICATE_EVERY --partitions=$PARTITIONS"
