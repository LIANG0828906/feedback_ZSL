#!/usr/bin/env python3
"""Run the local acceptance suite and write acceptance-report.json."""

from __future__ import annotations

import argparse
import json

from cover_replay.acceptance import run_acceptance


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="artifacts/acceptance")
    parser.add_argument("--seed", type=int, default=20261122)
    args = parser.parse_args()
    report = run_acceptance(args.out, seed=args.seed)
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report["ok"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
