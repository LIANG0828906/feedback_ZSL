#!/usr/bin/env python3
"""Run the local unit suite and end-to-end verification without network access."""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", default=str(ROOT / "samples"))
    parser.add_argument("--skip-unit", action="store_true")
    parser.add_argument("--skip-verify", action="store_true")
    arguments = parser.parse_args()
    if not arguments.skip_unit:
        completed = subprocess.run(
            [sys.executable, "-m", "unittest", "discover", "-s", "tests", "-v"],
            cwd=ROOT,
        )
        if completed.returncode != 0:
            return completed.returncode
    if not arguments.skip_verify:
        completed = subprocess.run(
            [
                sys.executable,
                "-m",
                "vp_pipeline.cli",
                "verify",
                "--samples",
                arguments.samples,
            ],
            cwd=ROOT,
        )
        if completed.returncode != 0:
            return completed.returncode
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
