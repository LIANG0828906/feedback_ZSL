#!/usr/bin/env python3
"""Generate a local development config without embedding the password."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=1433)
    parser.add_argument("--user", default="sa")
    parser.add_argument("--password-env", default="MSSQL_PASSWORD")
    parser.add_argument("--listen-port", type=int, default=8090)
    parser.add_argument("--max-connections", type=int, default=12)
    parser.add_argument("--output", default=str(ROOT / "config.local.json"))
    args = parser.parse_args()
    if not os.environ.get(args.password_env):
        raise SystemExit(f"environment variable {args.password_env} is required")
    base = (
        "server={host};port={port};user={user};"
        "password=${{{password_env}}};database={database};"
        "login_timeout=5;timeout=20"
    )
    config = {
        "listen": {"host": "127.0.0.1", "port": args.listen_port},
        "database": {
            "driver": "pymssql",
            "legacy_connection_string": base.format(
                host=args.host,
                port=args.port,
                user=args.user,
                password_env=args.password_env,
                database="OrdersMigrationLegacy",
            ),
            "shard_connection_string_template": base.format(
                host=args.host,
                port=args.port,
                user=args.user,
                password_env=args.password_env,
                database="OrdersMigrationShard{shard}",
            ),
            "shard_count": 4,
            "connect_timeout_seconds": 5,
            "command_timeout_seconds": 20,
        },
        "connection_pool": {
            "max_connections": args.max_connections,
            "acquire_timeout_ms": 250,
            "idle_ttl_seconds": 60,
        },
        "migration": {
            "batch_size": 500,
            "read_mode_during_migration": "legacy",
            "resume": True,
        },
        "reconciliation": {
            "batch_size": 500,
            "in_flight_retry_ms": 120,
            "in_flight_retry_limit": 8,
        },
        "rollback": {"wait_timeout_seconds": 30, "batch_size": 500},
        "audit": {"sqlite_path": "runtime/audit.db"},
        "load": {
            "target_writes_per_second": 20,
            "target_reads_per_second": 30,
            "write_p95_ms": 250,
            "read_p95_ms": 120,
        },
        "fault_injection": {"enabled": True, "admin_token": "${OM_ADMIN_TOKEN}"},
    }
    output = Path(args.output)
    output.write_text(
        json.dumps(config, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
