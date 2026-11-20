"""Command line interface for the offline level workbench."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from .canonical import read_json
from .errors import LevelGenError
from .injection import INJECTION_TYPES
from .service import (
    run_generation_to_directory,
    run_injection_to_directory,
    run_replay_to_directory,
)


def _load_object(path: str) -> dict[str, Any]:
    value = read_json(path)
    if not isinstance(value, dict):
        raise LevelGenError(
            "E_INPUT_INVALID",
            "输入文件必须是 JSON 对象",
            point=path,
        )
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m levelgen.cli")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser("generate")
    generate_parser.add_argument("--request", required=True)
    generate_parser.add_argument("--out", required=True)

    replay_parser = subparsers.add_parser("replay")
    replay_parser.add_argument("--trace", required=True)
    replay_parser.add_argument("--out", required=True)

    inject_parser = subparsers.add_parser("inject")
    inject_parser.add_argument("--request", required=True)
    inject_parser.add_argument("--type", required=True, choices=INJECTION_TYPES)
    inject_parser.add_argument("--out", required=True)

    samples_parser = subparsers.add_parser("samples")
    samples_parser.add_argument("--out", default="artifacts")

    verify_parser = subparsers.add_parser("verify")
    verify_parser.add_argument("--artifacts", default="artifacts")

    subparsers.add_parser("gui")
    return parser


def _print(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True))


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "generate":
            result = run_generation_to_directory(
                _load_object(args.request),
                args.out,
            )
            _print(
                {
                    "attempts": result.attempts,
                    "code": result.code,
                    "level_hash": (
                        None if result.level is None else result.level.content_hash
                    ),
                    "ok": result.ok,
                }
            )
            return 0 if result.ok else 2
        if args.command == "replay":
            payload = run_replay_to_directory(args.trace, args.out)
            _print(payload)
            return 0 if payload.get("ok") else 2
        if args.command == "inject":
            payload = run_injection_to_directory(
                _load_object(args.request),
                args.type,
                args.out,
            )
            _print(payload)
            return 0
        if args.command == "samples":
            from .samples import build_artifacts

            summary = build_artifacts(Path(args.out))
            _print(summary)
            return 0 if summary.get("ok") else 2
        if args.command == "verify":
            from .samples import verify_artifacts

            summary = verify_artifacts(Path(args.artifacts))
            _print(summary)
            return 0 if summary.get("ok") else 2
        if args.command == "gui":
            from .gui import main as gui_main

            return gui_main()
    except LevelGenError as error:
        _print(error.to_dict())
        return 3
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
