"""Command-line entry point for generation, rendering, verification, and diffs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .checkpoint import atomic_write_json
from .diff import compare_arrays, load_raw_output, write_diff_report
from .errors import SceneValidationError
from .pipeline import render_to_directory
from .scene import generate_scene, load_scene, write_scene
from .verify import verify_output


def _emit(value: Any, stream) -> None:
    stream.write(json.dumps(value, ensure_ascii=False, sort_keys=True) + "\n")
    stream.flush()


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="shadowbox",
        description="Deterministic offline validator for layered paper lightboxes",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="write the deterministic sample scene")
    generate.add_argument("--out", required=True, help="destination scene JSON")
    generate.add_argument("--width", type=int, default=320)
    generate.add_argument("--height", type=int, default=256)
    generate.add_argument("--block-width", type=int, default=64)
    generate.add_argument("--block-height", type=int, default=64)
    generate.add_argument("--samples-per-pixel", type=int, default=2)

    validate = subparsers.add_parser("validate", help="validate a scene without rendering")
    validate.add_argument("--scene", required=True)

    render = subparsers.add_parser("render", help="render a scene in deterministic blocks")
    render.add_argument("--scene", required=True)
    render.add_argument("--out", required=True, help="output directory")
    render.add_argument(
        "--fresh",
        action="store_true",
        help="discard any existing checkpoint instead of resuming",
    )
    render.add_argument(
        "--interrupt-after-blocks",
        type=int,
        default=None,
        help="finish this many new blocks, checkpoint, write a resume report, and exit 0",
    )
    render.add_argument(
        "--reference-dir",
        default=None,
        help="compare final pixels and depth against an uninterrupted output directory",
    )
    render.add_argument(
        "--error-report",
        default=None,
        help="optional destination for a structured validation error",
    )

    verify = subparsers.add_parser(
        "verify", help="rerender and verify image, optical depth, and report statistics"
    )
    verify.add_argument("--scene", required=True)
    verify.add_argument("--output", required=True)
    verify.add_argument(
        "--diff-report",
        default=None,
        help="diff report path (default: <output>/verify_diff.json)",
    )

    compare = subparsers.add_parser(
        "compare", help="compare two completed output directories"
    )
    compare.add_argument("--scene", required=True)
    compare.add_argument("--reference", required=True)
    compare.add_argument("--candidate", required=True)
    compare.add_argument("--report", required=True)
    return parser


def _handle_validation_error(
    error: SceneValidationError, error_report: str | None
) -> int:
    payload = error.as_dict()
    _emit(payload, sys.stderr)
    if error_report:
        atomic_write_json(error_report, payload)
    return 2


def _run(args: argparse.Namespace) -> int:
    if args.command == "generate":
        scene = generate_scene(
            width=args.width,
            height=args.height,
            block_size=(args.block_width, args.block_height),
            samples_per_pixel=args.samples_per_pixel,
        )
        write_scene(args.out, scene)
        _emit({"ok": True, "format": scene["format"]}, sys.stdout)
        return 0

    try:
        scene = load_scene(args.scene)
    except SceneValidationError as error:
        error_report = getattr(args, "error_report", None)
        return _handle_validation_error(error, error_report)

    if args.command == "validate":
        _emit(
            {
                "ok": True,
                "scene_sha256": scene.digest,
                "layers": [
                    {
                        "layer_index": layer.layer_index,
                        "name": layer.name,
                        "world_z_mm": layer.world_z_mm,
                    }
                    for layer in scene.layers_inner_to_outer
                ],
            },
            sys.stdout,
        )
        return 0

    if args.command == "render":
        result = render_to_directory(
            scene,
            args.out,
            fresh=args.fresh,
            interrupt_after_blocks=args.interrupt_after_blocks,
            reference_directory=args.reference_dir,
        )
        payload = {
            "ok": result.exit_code == 0,
            "interrupted": result.interrupted,
            "recovery_count": result.recovery_count,
            "discarded_checkpoints": result.discarded_checkpoints,
        }
        if result.report_path is not None:
            payload["report"] = result.report_path.name
        if result.resume_report_path is not None:
            payload["resume_report"] = result.resume_report_path.name
        if result.diff_report_path is not None:
            payload["diff_report"] = result.diff_report_path.name
        _emit(payload, sys.stdout)
        return result.exit_code

    if args.command == "verify":
        result = verify_output(scene, args.output)
        diff_path = (
            Path(args.diff_report)
            if args.diff_report
            else Path(args.output) / "verify_diff.json"
        )
        if not result["ok"] and not result["pixel_comparison"]["equal"]:
            write_diff_report(diff_path, result["pixel_comparison"])
        _emit(result, sys.stdout if result["ok"] else sys.stderr)
        return 0 if result["ok"] else 4

    if args.command == "compare":
        reference_rgba, reference_depth = load_raw_output(
            args.reference,
            scene.camera.width,
            scene.camera.height,
        )
        candidate_rgba, candidate_depth = load_raw_output(
            args.candidate,
            scene.camera.width,
            scene.camera.height,
        )
        comparison = compare_arrays(
            reference_rgba,
            candidate_rgba,
            reference_depth,
            candidate_depth,
            scene,
        )
        write_diff_report(args.report, comparison)
        _emit(comparison, sys.stdout if comparison["equal"] else sys.stderr)
        return 0 if comparison["equal"] else 4

    return 1


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        return _run(args)
    except (OSError, ValueError, RuntimeError) as error:
        _emit(
            {
                "ok": False,
                "code": "RENDER_ERROR",
                "errors": [
                    {
                        "code": "RENDER_ERROR",
                        "layer_index": None,
                        "message": str(error),
                    }
                ],
            },
            sys.stderr,
        )
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
