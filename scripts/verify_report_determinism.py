#!/usr/bin/env python3
"""Compare deterministic report fields from repeated runs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


TIME_KEYS = {
    "generated_at",
    "started_at",
    "finished_at",
    "updated_at",
    "entered_at",
    "exited_at",
    "recovered_at",
    "acknowledged_at",
    "elapsed_ms",
    "total_duration_ms",
}


def normalize(value):
    if isinstance(value, dict):
        return {
            key: normalize(child)
            for key, child in sorted(value.items())
            if key not in TIME_KEYS
        }
    if isinstance(value, list):
        return [normalize(item) for item in value]
    return value


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("first")
    parser.add_argument("second")
    args = parser.parse_args()
    first = normalize(json.loads(Path(args.first).read_text(encoding="utf-8")))
    second = normalize(json.loads(Path(args.second).read_text(encoding="utf-8")))
    if first == second:
        print("deterministic fields match")
        return 0
    print(json.dumps({"first": first, "second": second}, indent=2, sort_keys=True))
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
