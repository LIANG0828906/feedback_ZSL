#!/usr/bin/env python3
"""Generate the deterministic 250-segment sample archive."""

from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from wal_archive.samplegen import main  # noqa: E402


if __name__ == "__main__":
    sys.exit(main())
