#!/usr/bin/env python3
"""Convenience entry point: ``python app.py --root ARCHIVE --db CATALOG``."""

from __future__ import annotations

import sys

from wal_archive.cli import main


if __name__ == "__main__":
    sys.exit(main())
