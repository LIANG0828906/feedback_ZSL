from __future__ import annotations

import argparse
import json
import os
import sys
import traceback
from pathlib import Path
from typing import Any

from .errors import BusinessFailure, EXIT_RENDER
from .pipeline import render_project
from .synthesis import synthesize_manifest


def _print_json(value: Any) -> None:
    print(json.dumps(value, sort_keys=True, ensure_ascii=False, allow_nan=False))


def _emit_failure(failure: BusinessFailure) -> None:
    _print_json(
        {
            "event": "render_failed",
            "exit_code": failure.exit_code,
            "errors": [error.as_dict() for error in failure.errors],
        }
    )


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m mastering.cli",
        description="Deterministic offline broadcast stereo mastering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    synthesize = subparsers.add_parser(
        "synthesize",
        help="deterministically synthesize every WAV declared by the manifest",
    )
    synthesize.add_argument("--manifest", type=Path, required=True)

    render = subparsers.add_parser(
        "render",
        help="validate, align, duck, fold, master and report",
    )
    render.add_argument("--manifest", type=Path, required=True)
    resume_group = render.add_mutually_exclusive_group()
    resume_group.add_argument(
        "--resume",
        action="store_true",
        help="resume only from a committed snapshot matching current inputs",
    )
    resume_group.add_argument(
        "--fresh",
        action="store_true",
        help="discard any old snapshot and render from block zero (default)",
    )

    validate = subparsers.add_parser(
        "validate",
        help="validate manifest declarations and actual WAV metadata without rendering",
    )
    validate.add_argument("--manifest", type=Path, required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.command == "synthesize":
            report = synthesize_manifest(args.manifest)
            _print_json(
                {
                    "event": "synthesis_complete",
                    "manifest_digest": report["manifest_digest"],
                    "tracks": len(report["tracks"]),
                }
            )
            return 0
        if args.command == "validate":
            from .manifest import load_manifest

            parsed = load_manifest(args.manifest, validate_audio=True)
            _print_json(
                {
                    "event": "validation_ok",
                    "manifest_digest": parsed.manifest.digest,
                    "tracks": [track.name for track in parsed.manifest.tracks],
                }
            )
            return 0
        if args.command == "render":
            result = render_project(args.manifest, resume=bool(args.resume))
            _print_json(
                {
                    "event": "render_interrupted" if result.interrupted else "render_complete",
                    "exit_code": result.exit_code,
                    "report": str(result.report_path),
                    "snapshot": str(result.snapshot_path),
                    "completed_blocks": result.completed_blocks,
                    "total_blocks": result.total_blocks,
                }
            )
            return result.exit_code
        raise AssertionError(f"unhandled command: {args.command}")
    except BusinessFailure as failure:
        _emit_failure(failure)
        return failure.exit_code
    except KeyboardInterrupt:
        _print_json(
            {
                "event": "render_failed",
                "exit_code": EXIT_RENDER,
                "errors": [
                    {
                        "code": "E_INTERRUPTED_BEFORE_CHECKPOINT",
                        "message": "interrupt arrived before a checkpoint could be committed",
                    }
                ],
            }
        )
        return EXIT_RENDER
    except Exception as exc:  # pragma: no cover - final safety net
        error: dict[str, Any] = {
            "code": "E_INTERNAL",
            "message": f"{type(exc).__name__}: {exc}",
        }
        if os.environ.get("MASTERING_DEBUG"):
            error["traceback"] = traceback.format_exc()
        _print_json({"event": "render_failed", "exit_code": EXIT_RENDER, "errors": [error]})
        return EXIT_RENDER


if __name__ == "__main__":
    sys.exit(main())
