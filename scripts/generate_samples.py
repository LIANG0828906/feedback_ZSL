from __future__ import annotations

import argparse
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from balance_lab.generator import generate_all


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic Battle Balance Lab samples")
    parser.add_argument("--output", default=str(ROOT / "data" / "samples"))
    args = parser.parse_args()
    manifest = generate_all(args.output)
    print(f"generated {len(manifest['projects'])} projects")
    print(f"manifest_hash={manifest['manifest_hash']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

