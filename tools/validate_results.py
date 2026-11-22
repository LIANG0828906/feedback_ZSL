#!/usr/bin/env python3
"""Independent result validation: re-checks persisted interval results
against ground-truth fingerprints and reports the first mismatch."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from ocr_pipeline.validator import main

if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
