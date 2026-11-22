from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from sync_kernel.demo_runner import run_demo


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Run the deterministic local multiplayer sync demo."
    )
    parser.add_argument("--ticks", type=int, default=60)
    parser.add_argument("--gui", action="store_true", help="Open the PySide6 interface after validation")
    parser.add_argument(
        "--no-generate",
        action="store_true",
        help="Reuse existing deterministic samples instead of regenerating them",
    )
    parser.add_argument(
        "--workspace",
        type=Path,
        default=Path(__file__).resolve().parent,
    )
    args = parser.parse_args()
    if args.ticks < 30:
        parser.error("--ticks must be at least 30 to exercise checkpoint windows")
    report = run_demo(
        workspace=args.workspace,
        ticks=args.ticks,
        generate=not args.no_generate,
    )
    summary = report["summary"]
    print(
        f"demo: {summary['overall']} "
        f"({summary['passed']} passed, {summary['failed']} failed)"
    )
    print(
        "report: "
        f"{args.workspace / 'artifacts' / 'demo' / 'report.json'}"
    )
    print(f"final_hash: {report['convergence']['final_hash']}")
    if args.gui:
        from sync_kernel.ui import launch_ui

        return launch_ui(args.workspace / "samples")
    return 0 if summary["failed"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())

