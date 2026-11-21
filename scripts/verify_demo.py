#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from pcwb.demo import run_demo

DETERMINISTIC_PREFIXES = ("samples/", "repo/objects/")
DETERMINISTIC_FILES = ("trace.jsonl", "summary.json", "repo/manifest.json")


def tree(root: Path) -> dict[str, bytes]:
    result: dict[str, bytes] = {}
    for path in sorted(root.rglob("*")):
        if not path.is_file():
            continue
        relative = str(path.relative_to(root))
        if relative in DETERMINISTIC_FILES or relative.startswith(
            DETERMINISTIC_PREFIXES
        ):
            result[relative] = path.read_bytes()
    result.pop("repo/.lock", None)
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seed", type=int, default=20261122)
    args = parser.parse_args()
    with (
        tempfile.TemporaryDirectory() as first,
        tempfile.TemporaryDirectory() as second,
    ):
        first_root = Path(first) / "demo"
        second_root = Path(second) / "demo"
        run_demo(first_root, seed=args.seed, progress_threshold_ms=50.0)
        run_demo(second_root, seed=args.seed, progress_threshold_ms=50.0)
        first_files = tree(first_root)
        second_files = tree(second_root)
        all_names = sorted(set(first_files) | set(second_files))
        mismatches = [
            name
            for name in all_names
            if first_files.get(name) != second_files.get(name)
        ]
        report = {
            "seed": args.seed,
            "deterministic_files_compared": len(all_names),
            "byte_identical": not mismatches,
            "mismatches": mismatches,
            "excluded_intentionally": ["runtime_metrics.jsonl"],
        }
        print(json.dumps(report, indent=2, sort_keys=True))
        return 0 if not mismatches else 1


if __name__ == "__main__":
    raise SystemExit(main())
