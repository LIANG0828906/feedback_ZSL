#!/usr/bin/env python3
"""Create schema and deterministic seed data on both PostgreSQL instances."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from dualwrite.app import Runtime
from dualwrite.config import load_config


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="config.example.json")
    parser.add_argument("--members", type=int, default=20000)
    parser.add_argument("--orders", type=int, default=30000)
    parser.add_argument("--reset", action="store_true")
    args = parser.parse_args()

    config = load_config(args.config)
    runtime = Runtime(config)
    try:
        runtime.seed(args.members, args.orders, reset=args.reset)
    finally:
        runtime.databases.close()
    print(
        json.dumps(
            {
                "members": args.members,
                "orders": args.orders,
                "ok": True,
                "reset": args.reset,
                "sites": sorted(config.sites),
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

