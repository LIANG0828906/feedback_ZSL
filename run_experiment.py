"""Windows-friendly wrapper for the deterministic experiment CLI."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from tactical.cli import main

if __name__ == "__main__":
    raise SystemExit(main())
