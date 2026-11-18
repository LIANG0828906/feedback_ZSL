from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import threading
from pathlib import Path
from typing import Any, Sequence

from .errors import OutputMismatchError, SceneValidationError
from .output import compare_outputs
from .runner import run_render
from .sample import write_sample_scene
from .scene import load_scene
from .util import atomic_write_json


def _emit(value: dict[str, Any], *, stream: Any | None = None) -> None:
    target = sys.stdout if stream is None else stream
    target.write(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))
    target.write("\n")
    target.flush()


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="leather-raster",
        description="Deterministic offline soft rasterizer for layered leather shadow puppets.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample = subparsers.add_parser("sample", help="write the deterministic validation scene")
    sample.add_argument("--out", default="examples/sample_scene.json")
    sample.add_argument("--width", type=int, default=192)
    sample.add_argument("--height", type=int, default=144)
    sample.add_argument("--block-width", type=int, default=48)
    sample.add_argument("--block-height", type=int, default=36)
    sample.add_argument("--supersampling", type=int, default=2)
    sample.add_argument("--projection-samples", type=int, default=4)

    validate = subparsers.add_parser("validate", help="validate a scene JSON file")
    validate.add_argument("scene")
    validate.add_argument("--report", default=None)

    render = subparsers.add_parser("render", help="render or resume a chunked scene")
    render.add_argument("scene")
    render.add_argument("--out", default="output")
    render.add_argument("--fresh", action="store_true", help="discard the committed snapshot and restart")
    render.add_argument("--no-resume", action="store_true", help="do not load an existing snapshot")
    render.add_argument(
        "--interrupt-after-blocks",
        type=int,
        default=None,
        help="after N newly committed blocks, write a recovery report and exit 0",
    )
    render.add_argument("--verify-baseline", default=None)
    render.add_argument("--max-differences", type=int, default=512)

    resume = subparsers.add_parser("resume", help="resume an interrupted render")
    resume.add_argument("scene")
    resume.add_argument("--out", default="output")
    resume.add_argument("--verify-baseline", default=None)
    resume.add_argument("--max-differences", type=int, default=512)

    compare = subparsers.add_parser("compare", help="compare rendered outputs pixel by pixel")
    compare.add_argument("scene")
    compare.add_argument("--actual", required=True)
    compare.add_argument("--baseline", required=True)
    compare.add_argument("--max-differences", type=int, default=512)
    return parser


def _validation_failure(exc: SceneValidationError, report: str | None = None) -> int:
    payload = exc.to_dict()
    if report:
        atomic_write_json(report, payload)
    _emit(payload)
    return exc.exit_code


def _install_signal_flag() -> tuple[threading.Event, list[Any]]:
    stop_event = threading.Event()
    installed: list[tuple[int, Any]] = []

    def handler(_signum: int, _frame: Any) -> None:
        stop_event.set()

    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            signum = getattr(signal, name)
            try:
                previous = signal.signal(signum, handler)
                installed.append((signum, previous))
            except (OSError, ValueError):
                pass
    return stop_event, installed


def _restore_signals(installed: list[Any]) -> None:
    for signum, previous in installed:
        try:
            signal.signal(signum, previous)
        except (OSError, ValueError):
            pass


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "sample":
            scene = write_sample_scene(
                args.out,
                width=args.width,
                height=args.height,
                block_width=args.block_width,
                block_height=args.block_height,
                supersampling=args.supersampling,
                projection_samples=args.projection_samples,
            )
            _emit(
                {
                    "ok": True,
                    "command": "sample",
                    "path": args.out,
                    "layers": len(scene["layers"]),
                    "triangles_per_layer": [
                        len(layer["mesh"]["triangles"]) for layer in scene["layers"]
                    ],
                    "cutout_ids": [
                        [cutout["cutout_id"] for cutout in layer["cutouts"]]
                        for layer in scene["layers"]
                    ],
                }
            )
            return 0

        scene = load_scene(args.scene)
        if args.command == "validate":
            payload = {
                "ok": True,
                "scene_sha256": scene.digest,
                "layers": len(scene.layers),
                "triangle_counts": [len(layer.triangles) for layer in scene.layers],
                "cutout_counts": [len(layer.cutouts) for layer in scene.layers],
                "lights": [light.light_id for light in scene.lights],
                "image": [scene.width, scene.height],
            }
            if args.report:
                atomic_write_json(args.report, payload)
            _emit(payload)
            return 0

        if args.command in {"render", "resume"}:
            stop_event, installed = _install_signal_flag()
            try:
                outcome = run_render(
                    scene,
                    args.out,
                    resume=args.command == "resume" or not getattr(args, "no_resume", False),
                    fresh=getattr(args, "fresh", False),
                    interrupt_after_blocks=getattr(args, "interrupt_after_blocks", None),
                    stop_event=stop_event,
                    verify_baseline=args.verify_baseline,
                    max_differences=args.max_differences,
                )
            finally:
                _restore_signals(installed)
            _emit(
                {
                    "ok": outcome.exit_code == 0,
                    "command": args.command,
                    "status": outcome.status,
                    "exit_code": outcome.exit_code,
                    "completed_blocks": outcome.completed_blocks,
                    "total_blocks": outcome.total_blocks,
                    "rendered_blocks_this_run": outcome.rendered_this_run,
                    "loaded_blocks_at_start": outcome.loaded_blocks,
                    "snapshot_status": outcome.snapshot_status,
                    "snapshot_reason": outcome.snapshot_reason,
                    "recovery_report": outcome.report_path,
                    "output_dir": str(Path(args.out)),
                }
            )
            return outcome.exit_code

        if args.command == "compare":
            result = compare_outputs(
                scene,
                args.actual,
                args.baseline,
                max_differences=args.max_differences,
            )
            _emit(result)
            return 0

        raise RuntimeError(f"unknown command {args.command}")
    except SceneValidationError as exc:
        return _validation_failure(exc, getattr(args, "report", None))
    except OutputMismatchError as exc:
        _emit(
            {
                "ok": False,
                "error": {
                    "code": "OUTPUT_MISMATCH",
                    "message": exc.message,
                    "difference_count": len(exc.differences),
                    "first_differences": exc.differences[:8],
                },
            },
            stream=sys.stderr,
        )
        return exc.exit_code
    except Exception as exc:
        if os.environ.get("LEATHER_RASTER_DEBUG") == "1":
            raise
        _emit(
            {
                "ok": False,
                "error": {
                    "code": "INTERNAL_ERROR",
                    "message": f"{type(exc).__name__}: {exc}",
                },
            },
            stream=sys.stderr,
        )
        return 3

