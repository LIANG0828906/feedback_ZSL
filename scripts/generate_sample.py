#!/usr/bin/env python3
"""Generate the deterministic local sample."""

from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from formation_workbench.generator import generate_scene


def main() -> int:
    destination = Path("samples/formation_scene.json")
    destination.parent.mkdir(parents=True, exist_ok=True)
    payload = generate_scene(167)
    destination.write_text(
        json.dumps(payload, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(destination)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
