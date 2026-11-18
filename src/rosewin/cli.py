"""Command line entry points for the rose window validator."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

from .errors import RecoveryMismatchError, SceneValidationError
from .model import parse_scene
from .pipeline import RenderPipeline
from .scene import build_invalid_scene, build_scene, load_scene, write_scene
from .verify import compare_output_dirs, run_recovery_check, write_validation_error


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="rosewin", description="Deterministic rose window rasterizer")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate-scene", help="write the deterministic JSON scene")
    generate.add_argument("--output", required=True)
    generate.add_argument("--seed", type=int, default=208)
    generate.add_argument(
        "--invalid",
        choices=["index", "degenerate", "transmittance", "reflectance", "duplicate_quadrant", "duplicate_facet"],
        help="emit a deterministically broken scene instead",
    )

    render = sub.add_parser("render", help="render a scene, resuming from any valid snapshot")
    render.add_argument("--scene", required=True)
    render.add_argument("--output-dir", required=True)
    render.add_argument("--interrupt-after", type=int, default=None, help="stop after N chunks (exit 0)")

    verify = sub.add_parser("verify-recovery", help="baseline, interrupt, resume and compare bytewise")
    verify.add_argument("--scene", required=True)
    verify.add_argument("--workspace", required=True)
    verify.add_argument("--interrupt-after", type=int, default=2)

    validate = sub.add_parser("validate", help="validate a scene and emit a structured error report")
    validate.add_argument("--scene", required=True)
    validate.add_argument("--error-report", required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "generate-scene":
        scene = build_invalid_scene(args.invalid, args.seed) if args.invalid else build_scene(args.seed)
        write_scene(scene, args.output)
        return 0

    if args.command == "validate":
        scene = load_scene(args.scene)
        try:
            parse_scene(scene)
        except SceneValidationError as error:
            write_validation_error(error, args.error_report)
            return 2
        Path(args.error_report).write_text(
            json.dumps({"schema_version": "cathedral-rose-error/v1", "status": "valid", "issue_count": 0}),
            encoding="utf-8",
        )
        return 0

    if args.command == "render":
        scene = load_scene(args.scene)
        try:
            model = parse_scene(scene)
            artifacts = RenderPipeline(model, args.output_dir).run(
                interrupt_after_chunks=args.interrupt_after
            )
        except SceneValidationError as error:
            report_path = Path(args.output_dir) / "validation_error.json"
            write_validation_error(error, report_path)
            return 2
        return 0 if artifacts.status in {"complete", "interrupted"} else 3

    if args.command == "verify-recovery":
        scene = load_scene(args.scene)
        model = parse_scene(scene)
        try:
            report = run_recovery_check(model, args.workspace, interrupt_after=args.interrupt_after)
        except RecoveryMismatchError as error:
            workspace = Path(args.workspace)
            workspace.mkdir(parents=True, exist_ok=True)
            (workspace / "recovery_mismatch.json").write_text(
                json.dumps(error.to_payload(), indent=1, ensure_ascii=False), encoding="utf-8"
            )
            return 4
        return 0 if report["status"] == "pass" else 4

    return 1


if __name__ == "__main__":
    sys.exit(main())
