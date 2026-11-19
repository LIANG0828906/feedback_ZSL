#!/usr/bin/env bash
# Optional helper for POSIX shells. The service itself is cross-platform.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONPATH="$ROOT/src:${PYTHONPATH:-}"
export SLOTGUARD_PRIMARY_DSN="${SLOTGUARD_PRIMARY_DSN:-host=127.0.0.1 port=55432 user=postgres dbname=postgres}"
export SLOTGUARD_REPLICA_DSN="${SLOTGUARD_REPLICA_DSN:-host=127.0.0.1 port=55433 user=postgres dbname=postgres}"
export SLOTGUARD_REDIS_URL="${SLOTGUARD_REDIS_URL:-redis://127.0.0.1:6379/0}"
