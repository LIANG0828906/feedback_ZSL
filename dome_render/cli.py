"""Command-line interface."""

from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path

from . import __version__
from .errors import RenderFailure, ValidationFailure
from .pipeline import InterruptFlag, run
from .scene import load_scene


def _print_json(value: object) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="dome-render", description="Deterministic offline layered-dome soft renderer.")
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    subparsers = parser.add_subparsers(dest="command", required=True)

    render_parser = subparsers.add_parser("render", help="Render a scene and write deterministic artifacts.")
    render_parser.add_argument("--scene", required=True, help="Input scene JSON.")
    render_parser.add_argument("--out", required=True, help="Output directory.")
    render_parser.add_argument("--fresh", action="store_true", help="Discard a partial snapshot and start from zero.")
    render_parser.add_argument("--quiet", action="store_true", help="Suppress per-block stderr progress.")

    validate_parser = subparsers.add_parser("validate", help="Validate a scene without rendering.")
    validate_parser.add_argument("--scene", required=True, help="Input scene JSON.")
    args = parser.parse_args(argv)

    try:
        scene = load_scene(args.scene)
    except ValidationFailure as exc:
        _print_json(exc.as_dict())
        return 2

    if args.command == "validate":
        _print_json({"ok": True, "scene": scene.summary})
        return 0

    interrupt = InterruptFlag()

    def handle_interrupt(signum: int, frame: object) -> None:
        del signum, frame
        interrupt.requested = True

    previous_handler = signal.signal(signal.SIGINT, handle_interrupt)
    try:
        return run(
            scene,
            Path(args.out),
            fresh=bool(args.fresh),
            interrupt=interrupt,
            progress=not bool(args.quiet),
        )
    except RenderFailure as exc:
        _print_json(exc.as_dict())
        return 5 if exc.code == "E_RESUME_MISMATCH" else 3
    except OSError as exc:
        _print_json(
            {
                "ok": False,
                "error": {
                    "code": "E_OUTPUT_IO",
                    "message": f"Output I/O failure: {exc.__class__.__name__}",
                },
            }
        )
        return 3
    finally:
        signal.signal(signal.SIGINT, previous_handler)


if __name__ == "__main__":
    raise SystemExit(main())
