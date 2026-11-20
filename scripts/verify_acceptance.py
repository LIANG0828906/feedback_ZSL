#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mesh_editor.acceptance import run_acceptance


def main() -> int:
    parser = argparse.ArgumentParser(description="Run reproducible local acceptance flow")
    parser.add_argument("--root", default=str(ROOT))
    args = parser.parse_args()
    report = run_acceptance(args.root)
    print(json.dumps(report, sort_keys=True, indent=2, ensure_ascii=False))
    return 0 if report["all_pass"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
