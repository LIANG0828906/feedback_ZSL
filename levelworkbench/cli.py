"""Command-line entry point for generation, replay and injection tests."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .config import SUPPORTED_INJECTIONS, load_config
from .errors import (
    E_BUSY,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_CONSTRAINT_CONFLICT,
    E_CONSTRAINT_INVALID,
    E_GENERATION_FAILED,
    E_INJECT_CONSTRAINT_LIMIT,
    E_INJECT_DANGLING_DOOR,
    E_INJECT_KEY_CYCLE,
    E_INJECT_ROOM_BOUNDS,
    E_INJECT_TRUNCATED_INPUT,
    E_REPLAY_MISMATCH,
    E_TRACE_INVALID,
    WorkbenchError,
)
from .pipeline import generate_from_config, replay_trace, write_generation_artifacts


EXIT_CODES = {
    E_CONFIG_INVALID: 2,
    E_CONSTRAINT_INVALID: 2,
    E_CONSTRAINT_CONFLICT: 3,
    E_GENERATION_FAILED: 3,
    E_TRACE_INVALID: 4,
    E_REPLAY_MISMATCH: 4,
    E_BUSY: 5,
    E_INJECT_ROOM_BOUNDS: 6,
    E_INJECT_DANGLING_DOOR: 6,
    E_INJECT_KEY_CYCLE: 6,
    E_INJECT_CONSTRAINT_LIMIT: 6,
    E_INJECT_TRUNCATED_INPUT: 6,
    E_CANCELLED: 130,
}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="levelworkbench",
        description="Deterministic offline level generation workbench",
    )
    parser.add_argument("--config", type=Path, help="constraint JSON file")
    parser.add_argument(
        "--replay",
        nargs="?",
        const="__auto__",
        metavar="TRACE",
        help="replay a trace; defaults to <out>/trace.json or artifacts/run/trace.json",
    )
    parser.add_argument("--gui", action="store_true", help="open the PySide6 workbench")
    parser.add_argument(
        "--inject",
        choices=sorted(SUPPORTED_INJECTIONS),
        help="parser-stage anomaly injection",
    )
    parser.add_argument("--out", type=Path, default=None, help="artifact output directory")
    parser.add_argument("--no-write", action="store_true", help="do not write level/trace files")
    parser.add_argument("--pretty", action="store_true", help="pretty-print CLI JSON")
    return parser


def _print_json(payload: dict[str, Any], pretty: bool) -> None:
    if pretty:
        print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))
    else:
        print(json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")))


def _resolve_out(args: argparse.Namespace, default_name: str) -> Path:
    return args.out if args.out is not None else Path("artifacts") / default_name


def run(args: argparse.Namespace) -> int:
    if args.gui:
        if args.config is None:
            raise WorkbenchError(
                E_CONFIG_INVALID,
                "--gui requires --config",
            )
        from .gui import run_gui

        return run_gui(args.config, injection=args.inject)

    replay_arg = args.replay
    if replay_arg is not None:
        out_dir = _resolve_out(args, "replay")
        if replay_arg == "__auto__":
            trace_path = (
                args.out / "trace.json"
                if args.out is not None
                else Path("artifacts/run/trace.json")
            )
        else:
            trace_path = Path(replay_arg)
        replay_result = replay_trace(trace_path)
        payload = replay_result.summary()
        if not args.no_write:
            committed = write_generation_artifacts(
                replay_result.generation,
                out_dir,
                level_name="replayed_level.json",
                trace_name="replayed_trace.json",
            )
            payload["output_files"] = committed
        _print_json(payload, args.pretty)
        return 0

    if args.config is None:
        raise WorkbenchError(
            E_CONFIG_INVALID,
            "--config is required unless --replay is used",
        )
    config = load_config(args.config, injection=args.inject)
    if args.inject:
        raise AssertionError("injection must fail before generation")
    result = generate_from_config(config)
    out_dir = _resolve_out(args, "run")
    payload = result.summary()
    if not args.no_write:
        committed = write_generation_artifacts(result, out_dir)
        payload["output_files"] = committed
    _print_json(payload, args.pretty)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return run(args)
    except WorkbenchError as exc:
        _print_json(exc.to_dict(), args.pretty)
        return EXIT_CODES.get(exc.code, 1)
    except Exception as exc:  # pragma: no cover - last-resort CLI boundary
        _print_json(
            {
                "ok": False,
                "code": "E_INTERNAL",
                "message": f"{type(exc).__name__}: {exc}",
            },
            args.pretty,
        )
        return 1
