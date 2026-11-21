"""Console entry point with stable process exit codes."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from .errors import BUSINESS_EXIT_CODES, ToolError
from .generator import generate_samples
from .injection import INJECTION_CASES, injection_selfcheck, trigger_injection
from .regression import run_regression
from .util import canonical_json


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="tactical-debug",
        description="Deterministic local tactical picking and regression suite",
    )
    parser.add_argument(
        "command",
        nargs="?",
        choices=("run", "generate", "regress", "injections"),
        default="run",
        help="run generation+regression, generate only, regress existing samples, or self-check injections",
    )
    parser.add_argument("--samples", default="samples", help="sample root directory")
    parser.add_argument("--out", default="out", help="output root directory")
    parser.add_argument("--seed", type=int, default=20261122, help="deterministic sample seed")
    parser.add_argument(
        "--inject",
        choices=INJECTION_CASES,
        help="inject one input-entry fault and return its business code before writing output",
    )
    return parser


def _print(value: Any) -> None:
    print(canonical_json(value))


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)

    if args.inject:
        # This branch runs before sample/output path creation.
        error = trigger_injection(args.inject, args.seed)
        _print(error.to_dict())
        return error.exit_code

    try:
        if args.command == "generate":
            manifest = generate_samples(args.samples, args.seed)
            _print(
                {
                    "ok": True,
                    "business_code": "OK",
                    "exit_code": 0,
                    "command": "generate",
                    "sample_root": str(Path(args.samples) / f"seed_{args.seed}"),
                    "cases": len(manifest["cases"]),
                    "seed": args.seed,
                }
            )
            return 0

        if args.command == "regress":
            report = run_regression(args.samples, args.out, args.seed)
            _print(
                {
                    "ok": True,
                    "business_code": "OK",
                    "exit_code": 0,
                    "command": "regress",
                    "report": str(Path(args.out) / f"seed_{args.seed}" / "report.json"),
                    "totals": report["totals"],
                    "concurrency": report["concurrency"],
                }
            )
            return 0

        if args.command == "injections":
            result = injection_selfcheck(args.seed)
            _print({"business_code": "OK" if result["ok"] else "E_CHECK_FAILED", "exit_code": 0 if result["ok"] else BUSINESS_EXIT_CODES["E_CHECK_FAILED"], **result})
            return 0 if result["ok"] else BUSINESS_EXIT_CODES["E_CHECK_FAILED"]

        generate_samples(args.samples, args.seed)
        report = run_regression(args.samples, args.out, args.seed)
        _print(
            {
                "ok": True,
                "business_code": "OK",
                "exit_code": 0,
                "command": "run",
                "sample_root": str(Path(args.samples) / f"seed_{args.seed}"),
                "report": str(Path(args.out) / f"seed_{args.seed}" / "report.json"),
                "totals": report["totals"],
                "concurrency": report["concurrency"],
            }
        )
        return 0
    except ToolError as exc:
        _print(exc.to_dict())
        return exc.exit_code
    except Exception as exc:  # pragma: no cover - final safety net
        error = ToolError("E_INTERNAL", f"unexpected internal error: {exc}", "cli")
        _print(error.to_dict())
        return error.exit_code


if __name__ == "__main__":
    raise SystemExit(main())

