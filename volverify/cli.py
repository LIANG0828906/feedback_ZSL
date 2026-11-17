from __future__ import annotations

import argparse
import json
import signal
import sys
import traceback
from pathlib import Path
from typing import Sequence

from .compare import compare_render_dirs, write_comparison
from .io_utils import atomic_write_json, pretty_json_text
from .pipeline import (
    render_batch,
    render_scene,
    validate_scene_only,
)
from .runtime import StopController
from .sample_data import generate_sample_suite
from .validation import load_validated_scene
from .errors import FrameRejected


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="volverify",
        description=(
            "Deterministic offline volume-rendering verification with "
            "validation, convergence checks, and resumable chunk snapshots."
        ),
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample = subparsers.add_parser(
        "sample", help="generate deterministic local sample scenes"
    )
    sample.add_argument("--out", required=True, type=Path)
    sample.add_argument(
        "--dims", default="56,44,36", help="volume dims as nx,ny,nz"
    )
    sample.add_argument("--width", type=int, default=64)
    sample.add_argument("--height", type=int, default=48)

    validate = subparsers.add_parser(
        "validate", help="validate one scene and print structured results"
    )
    validate.add_argument("--scene", required=True, type=Path)
    validate.add_argument("--report", type=Path)

    render = subparsers.add_parser(
        "render", help="validate, check convergence, and render one frame"
    )
    render.add_argument("--scene", required=True, type=Path)
    render.add_argument("--out", required=True, type=Path)
    render.add_argument("--resume", action="store_true")
    render.add_argument(
        "--compare-full",
        type=Path,
        help="compare the final incremental/full result with a baseline directory",
    )

    resume = subparsers.add_parser(
        "resume", help="resume one frame from its persisted chunk state"
    )
    resume.add_argument("--scene", required=True, type=Path)
    resume.add_argument("--out", required=True, type=Path)
    resume.add_argument(
        "--compare-full",
        type=Path,
        help="compare the resumed result with a full-render baseline directory",
    )

    batch = subparsers.add_parser(
        "batch", help="render a manifest and continue past rejected frames"
    )
    batch.add_argument("--manifest", required=True, type=Path)
    batch.add_argument("--out", required=True, type=Path)
    batch.add_argument("--resume", action="store_true")

    compare = subparsers.add_parser(
        "compare",
        help="compare two render directories and write a structured diff",
    )
    compare.add_argument("--scene", required=True, type=Path)
    compare.add_argument("--left", required=True, type=Path)
    compare.add_argument("--right", required=True, type=Path)
    compare.add_argument("--report", required=True, type=Path)
    compare.add_argument("--max-samples-per-chunk", type=int, default=1000)

    return parser


def _parse_dims(value: str) -> tuple[int, int, int]:
    parts = [part.strip() for part in value.split(",")]
    if len(parts) != 3:
        raise argparse.ArgumentTypeError("dims must have three components")
    dims = tuple(int(part) for part in parts)
    if any(component <= 0 for component in dims):
        raise argparse.ArgumentTypeError("dims must be positive")
    return dims  # type: ignore[return-value]


def _install_signal_handlers(
    controller: StopController,
) -> dict[int, object]:
    previous: dict[int, object] = {}

    def handler(signal_number: int, _frame: object) -> None:
        if controller.signal_number is None:
            controller.signal_number = signal_number

    for signal_name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, signal_name):
            signal_number = int(getattr(signal, signal_name))
            previous[signal_number] = signal.getsignal(signal_number)
            signal.signal(signal_number, handler)
    return previous


def _restore_signal_handlers(previous: dict[int, object]) -> None:
    for signal_number, handler in previous.items():
        signal.signal(signal_number, handler)


def _emit(value: object) -> None:
    sys.stdout.write(pretty_json_text(value))
    sys.stdout.flush()


def main(argv: Sequence[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    controller = StopController()
    previous_handlers = _install_signal_handlers(controller)
    try:
        if args.command == "sample":
            dims = _parse_dims(args.dims)
            summary = generate_sample_suite(
                args.out, dims=dims, width=args.width, height=args.height
            )
            _emit(summary)
            return 0

        if args.command == "validate":
            report = validate_scene_only(args.scene)
            if args.report is not None:
                atomic_write_json(args.report, report)
            _emit(report)
            return 0 if report["status"] == "valid" else 2

        if args.command == "render":
            outcome = render_scene(
                args.scene,
                args.out,
                resume=args.resume,
                should_stop=controller,
                compare_full=args.compare_full,
            )
            _emit(outcome.report)
            return outcome.exit_code

        if args.command == "resume":
            outcome = render_scene(
                args.scene,
                args.out,
                resume=True,
                should_stop=controller,
                compare_full=args.compare_full,
            )
            _emit(outcome.report)
            return outcome.exit_code

        if args.command == "batch":
            exit_code, report = render_batch(
                args.manifest,
                args.out,
                resume=args.resume,
                should_stop=controller,
            )
            _emit(report)
            return exit_code

        if args.command == "compare":
            try:
                scene = load_validated_scene(args.scene)
            except FrameRejected as rejected:
                report = {
                    "schema_version": 1,
                    "status": "error",
                    "code": "SCENE_REJECTED",
                    "frame_id": rejected.frame_id,
                    "issues": [
                        issue.to_dict() for issue in rejected.issues
                    ],
                    "chunks": [],
                }
                write_comparison(report, args.report)
                _emit(report)
                return 2
            report = compare_render_dirs(
                scene,
                args.left,
                args.right,
                max_samples_per_chunk=args.max_samples_per_chunk,
            )
            write_comparison(report, args.report)
            _emit(report)
            return 0 if report["status"] == "identical" else 4

        parser.error(f"unknown command {args.command}")
        return 2
    except Exception as exc:  # pragma: no cover - defensive CLI boundary
        error_report = {
            "schema_version": 1,
            "status": "error",
            "code": "UNEXPECTED_EXCEPTION",
            "message": str(exc),
        }
        _emit(error_report)
        traceback.print_exc(file=sys.stderr)
        return 1
    finally:
        _restore_signal_handlers(previous_handlers)

