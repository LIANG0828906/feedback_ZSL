#!/usr/bin/env python3
"""Convenience wrapper around `python -m minute_backfill.cli run`."""

from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from minute_backfill.cli import main


if __name__ == "__main__":
    raise SystemExit(main(["run", *sys.argv[1:]]))
