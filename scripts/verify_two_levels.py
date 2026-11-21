#!/usr/bin/env python3
"""Run the two-level acceptance verification and print a compact summary."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from lightprobe.verify import verify_two_levels


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", default="samples")
    parser.add_argument("--report", default="artifacts/reports/verification.json")
    args = parser.parse_args()
    report = verify_two_levels(args.samples, output_path=args.report)
    print(json.dumps(report, ensure_ascii=False, indent=2))
    raise SystemExit(0 if report["ok"] else 1)


if __name__ == "__main__":
    main()

