#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mesh_editor.samplegen import generate_project_samples


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate local deterministic mesh samples and baseline")
    parser.add_argument("--root", default=str(ROOT))
    parser.add_argument("--weld-tolerance", type=float, default=1e-6)
    parser.add_argument("--keep-workspace", action="store_true")
    args = parser.parse_args()
    report = generate_project_samples(
        args.root,
        weld_tolerance=args.weld_tolerance,
        reset_workspace=not args.keep_workspace,
    )
    print(json.dumps(report, sort_keys=True, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
