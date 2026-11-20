"""Command-line entry point for generation, run, and report validation."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .config import load_workbench_config
from .errors import ErrorCode, MeterError
from .generator import generate_samples
from .snapshot import SnapshotStore
from .workbench import run_workbench


def _print(payload: Any) -> None:
    print(json.dumps(payload, sort_keys=True, indent=2, ensure_ascii=False))


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="loudness-workbench",
        description="Deterministic multi-program LUFS/true-peak workbench",
    )
    subparsers = parser.add_subparsers(dest="command")

    generate = subparsers.add_parser("generate", help="generate deterministic WAV samples")
    generate.add_argument("--sample-dir", default="samples")
    generate.add_argument("--config", default="samples/config.json")
    generate.add_argument("--seed", type=int, default=20261121)

    run = subparsers.add_parser("run", help="run parallel measurement and verification")
    run.add_argument("--config", default="samples/config.json")
    run.add_argument("--skip-offline", action="store_true")

    verify = subparsers.add_parser("verify", help="verify a disk snapshot hash chain")
    verify.add_argument("--snapshot", required=True)
    verify.add_argument("--parameter-version", type=int, default=None)

    gui = subparsers.add_parser("gui", help="open the optional Tk workbench")
    gui.add_argument("--config", default="samples/config.json")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    command = args.command or "run"
    try:
        if command == "generate":
            _print(
                generate_samples(
                    args.sample_dir,
                    seed=args.seed,
                    config_path=args.config,
                )
            )
            return 0
        if command == "run":
            config = load_workbench_config(args.config)
            result = run_workbench(
                config,
                run_offline_check=not args.skip_offline,
            )
            _print(result)
            return 0 if result.get("ok") else 1
        if command == "verify":
            store = SnapshotStore(args.snapshot)
            report = store.load(
                expected_parameter_version=args.parameter_version,
            )
            _print(
                {
                    "ok": True,
                    "asset_id": report.asset_id,
                    "blocks": report.block_count,
                    "parameter_version": report.parameter_version,
                    "head_hash": report.head_hash,
                    "report_hash": report.report_hash,
                }
            )
            return 0
        if command == "gui":
            from .gui import run_gui

            return run_gui(args.config)
    except MeterError as exc:
        _print(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        _print(
            {
                "ok": False,
                "code": ErrorCode.E_TASK_FAILED,
                "message": "interrupted",
            }
        )
        return 130
    parser.print_help()
    return 2
