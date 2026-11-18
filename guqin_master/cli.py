from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .compare import compare_master_files
from .errors import BusinessError
from .pipeline import run_project
from .reporting import stable_json_text
from .synth import write_demo_project


def _print(value: Any) -> None:
    sys.stdout.write(stable_json_text(value))
    sys.stdout.flush()


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="guqin-master",
        description="Deterministic offline guqin multitrack mastering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    demo = subparsers.add_parser(
        "generate-demo", help="generate deterministic six-channel material"
    )
    demo.add_argument("--out", type=Path, default=Path("demo_project"))
    demo.add_argument("--seed", type=int, default=20261118)

    run = subparsers.add_parser("run", help="render and report a project")
    run.add_argument("--project", type=Path, required=True)
    run.add_argument("--out", type=Path, required=True)
    run.add_argument("--quiet-progress", action="store_true")

    compare = subparsers.add_parser(
        "compare", help="compare two master WAV files byte-for-byte"
    )
    compare.add_argument("--left", type=Path, required=True)
    compare.add_argument("--right", type=Path, required=True)
    compare.add_argument("--block-size", type=int, default=4096)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate-demo":
            manifest_path = write_demo_project(args.out, seed=args.seed)
            summary = {
                "business_code": "OK",
                "status": "GENERATED",
                "manifest": manifest_path.relative_to(args.out).as_posix(),
                "seed": args.seed,
            }
            _print(summary)
            return 0
        if args.command == "run":
            reporter = None
            if not args.quiet_progress:
                reporter = lambda value: _print(value)
            result = run_project(args.project, args.out, reporter)
            _print(result)
            return 0 if result["business_code"] == "OK" else 1
        if args.command == "compare":
            compare_master_files(args.left, args.right, args.block_size)
            _print(
                {
                    "business_code": "OK",
                    "status": "IDENTICAL",
                    "byte_identical": True,
                }
            )
            return 0
    except BusinessError as exc:
        _print(exc.as_dict())
        return exc.exit_code
    except KeyboardInterrupt:
        _print(
            {
                "business_code": "E_IO",
                "channel": "project",
                "message": "interrupted before the current render block could be committed",
            }
        )
        return 6
    except Exception as exc:
        _print(
            {
                "business_code": "E_IO",
                "channel": "project",
                "message": f"unexpected rendering failure: {exc}",
            }
        )
        return 6
    parser.error("unreachable")
    return 2
