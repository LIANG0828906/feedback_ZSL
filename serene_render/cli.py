from __future__ import annotations

import argparse
import json
from pathlib import Path
import signal
import sys
import threading
from typing import Any

from .compare import compare_outputs
from .errors import RenderError, SceneValidationError
from .pipeline import run_render
from .scene import write_sample_scene


def _print_json(value: Any, *, stream=sys.stdout) -> None:
    print(
        json.dumps(
            value,
            indent=2,
            sort_keys=True,
            ensure_ascii=False,
            allow_nan=False,
        ),
        file=stream,
    )


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="serene-render",
        description=(
            "Deterministic offline soft-raster renderer for a layered "
            "three-silk/two-lattice garden door."
        ),
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample = subparsers.add_parser(
        "sample",
        help="write the deterministic five-layer sample scene JSON",
    )
    sample.add_argument(
        "--output",
        default="examples/garden_door.json",
        help="output scene JSON path",
    )

    render = subparsers.add_parser(
        "render",
        help="validate a scene and render with resumable tile snapshots",
    )
    render.add_argument("--scene", required=True, help="scene JSON path")
    render.add_argument("--out", required=True, help="output directory")
    render.add_argument(
        "--fresh",
        action="store_true",
        help="discard any checkpoint and render from tile zero",
    )
    render.add_argument(
        "--verify-baseline",
        default=None,
        metavar="DIR",
        help="after completion, compare every pixel with an uninterrupted run",
    )
    render.add_argument(
        "--block-delay-ms",
        type=int,
        default=0,
        help=argparse.SUPPRESS,
    )

    compare = subparsers.add_parser(
        "compare",
        help="compare two completed output directories",
    )
    compare.add_argument("--reference", required=True)
    compare.add_argument("--candidate", required=True)
    compare.add_argument("--max-differences", type=int, default=200)

    return parser


def _run_command(args: argparse.Namespace) -> int:
    if args.command == "sample":
        path = write_sample_scene(Path(args.output))
        _print_json({"ok": True, "status": "sample_written", "path": str(path)})
        return 0

    if args.command == "compare":
        differences = compare_outputs(
            args.reference,
            args.candidate,
            max_differences=max(1, int(args.max_differences)),
        )
        if differences:
            _print_json(
                {
                    "ok": False,
                    "code": "RESUME_OUTPUT_MISMATCH",
                    "differences": differences,
                },
                stream=sys.stderr,
            )
            return 4
        _print_json({"ok": True, "status": "identical"})
        return 0

    if args.command == "render":
        stop_event = threading.Event()
        previous_handlers: dict[int, Any] = {}

        def request_stop(signum, frame) -> None:
            stop_event.set()

        for signum in (signal.SIGINT, signal.SIGTERM):
            try:
                previous_handlers[signum] = signal.getsignal(signum)
                signal.signal(signum, request_stop)
            except (ValueError, OSError):
                pass

        try:
            result = run_render(
                args.scene,
                args.out,
                fresh=bool(args.fresh),
                stop_event=stop_event,
                block_delay_ms=max(0, int(args.block_delay_ms)),
                verify_baseline=args.verify_baseline,
            )
        finally:
            for signum, handler in previous_handlers.items():
                try:
                    signal.signal(signum, handler)
                except (ValueError, OSError):
                    pass

        _print_json(
            {
                "ok": True,
                "status": result.status,
                "scene_digest": result.scene_digest,
                "output_dir": str(result.output_dir),
                "completed_blocks": result.completed_blocks,
                "tile_count": result.tile_count,
                "report": (
                    str(result.report_path) if result.report_path is not None else None
                ),
            }
        )
        return 0

    raise AssertionError(f"unknown command {args.command}")


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        return _run_command(args)
    except SceneValidationError as exc:
        _print_json(exc.payload(), stream=sys.stderr)
        return exc.exit_code
    except RenderError as exc:
        _print_json(exc.payload(), stream=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        _print_json(
            {
                "ok": False,
                "code": "UNHANDLED_KEYBOARD_INTERRUPT",
                "message": "interrupt arrived outside the resumable tile loop",
            },
            stream=sys.stderr,
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
