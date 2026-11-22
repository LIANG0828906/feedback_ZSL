#!/usr/bin/env python3
"""Convenience wrapper for the packaged acceptance runner."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from filter_studio.acceptance import main

raise SystemExit(main())
