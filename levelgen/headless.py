"""Display-free entry point for generate and replay workflows."""

from __future__ import annotations

import argparse
import json
from typing import Any

from .canonical import read_json
from .errors import LevelGenError
from .service import (
    run_generation_to_directory,
    run_replay_to_directory,
)


def _load_request(path: str) -> dict[str, Any]:
    value = read_json(path)
    if not isinstance(value, dict):
        raise LevelGenError(
            "E_INPUT_INVALID",
            "请求文件必须是 JSON 对象",
            point="request",
        )
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Levelgen headless workflows")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser("generate")
    generate_parser.add_argument("--request", required=True)
    generate_parser.add_argument("--out", required=True)

    replay_parser = subparsers.add_parser("replay")
    replay_parser.add_argument("--trace", required=True)
    replay_parser.add_argument("--out", required=True)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "generate":
            result = run_generation_to_directory(
                _load_request(args.request),
                args.out,
            )
            summary = {
                "attempts": result.attempts,
                "code": result.code,
                "level_hash": (
                    None if result.level is None else result.level.content_hash
                ),
                "ok": result.ok,
            }
            print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
            return 0 if result.ok else 2
        if args.command == "replay":
            payload = run_replay_to_directory(args.trace, args.out)
            print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
            return 0 if payload.get("ok") else 2
    except LevelGenError as error:
        print(json.dumps(error.to_dict(), ensure_ascii=False, sort_keys=True))
        return 3
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
