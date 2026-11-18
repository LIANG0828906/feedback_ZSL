from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from . import __version__
from .compare import compare_outputs
from .engine import run_render
from .errors import SceneError
from .io_utils import write_json
from .sample_scene import write_sample_scene
from .scene import load_scene


def main(argv: Sequence[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "validate":
            return _command_validate(args)
        if args.command == "render":
            return _command_render(args)
        if args.command == "make-scene":
            return _command_make_scene(args)
        if args.command == "compare":
            return _command_compare(args)
        if args.command == "verify-resume":
            return _command_verify_resume(args)
    except SceneError as exc:
        payload = exc.as_dict()
        _print_json(payload, stream=sys.stderr)
        return 2
    parser.print_help()
    return 2


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="bamboo-render",
        description="Deterministic offline software renderer for a three-layer bamboo lampshade",
    )
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    validate = subparsers.add_parser("validate", help="validate a scene JSON file")
    validate.add_argument("scene", type=Path)
    validate.add_argument("--report", type=Path)

    render = subparsers.add_parser("render", help="render a validated scene in deterministic chunks")
    render.add_argument("scene", type=Path)
    render.add_argument("--out", type=Path, required=True)
    render.add_argument(
        "--interrupt-after",
        type=int,
        default=None,
        help="commit exactly N new chunks, then exit successfully with a resumable snapshot",
    )
    render.add_argument(
        "--restart",
        action="store_true",
        help="discard any existing snapshot and recompute from block zero",
    )

    make_scene = subparsers.add_parser("make-scene", help="write the deterministic acceptance scene")
    make_scene.add_argument("--out", type=Path, required=True)
    make_scene.add_argument("--width", type=int, default=320)
    make_scene.add_argument("--height", type=int, default=240)
    make_scene.add_argument("--chunk-width", type=int, default=64)
    make_scene.add_argument("--chunk-height", type=int, default=48)

    compare = subparsers.add_parser("compare", help="compare two completed render directories")
    compare.add_argument("--reference", type=Path, required=True)
    compare.add_argument("--candidate", type=Path, required=True)
    compare.add_argument("--report", type=Path, required=True)

    verify = subparsers.add_parser(
        "verify-resume",
        help="render once, interrupt, resume, and compare everything byte for byte",
    )
    verify.add_argument("scene", type=Path)
    verify.add_argument("--out", type=Path, required=True)
    verify.add_argument("--interrupt-after", type=int, default=7)
    verify.add_argument("--report", type=Path)
    return parser


def _command_validate(args: argparse.Namespace) -> int:
    loaded = load_scene(args.scene)
    report = {
        "ok": True,
        "scene_name": loaded.spec.scene_name,
        "scene_digest": loaded.scene_digest,
        "layers": [
            {
                "layer": layer.layer,
                "name": layer.name,
                "air_gap": layer.air_gap,
                "plane_z": layer.plane_z,
                "triangles": int(layer.triangles.shape[0]),
                "texture_resolution": list(layer.texture_resolution),
            }
            for layer in loaded.spec.layers
        ],
        "image": {
            "width": loaded.spec.image.width,
            "height": loaded.spec.image.height,
            "chunk_size": [
                loaded.spec.image.chunk_width,
                loaded.spec.image.chunk_height,
            ],
            "block_count": loaded.spec.image.block_count,
        },
    }
    if args.report is not None:
        write_json(args.report, report)
    _print_json(report)
    return 0


def _command_render(args: argparse.Namespace) -> int:
    loaded = load_scene(args.scene)
    outcome = run_render(
        loaded,
        args.out,
        interrupt_after=args.interrupt_after,
        restart=args.restart,
    )
    payload: dict[str, Any] = {
        "ok": True,
        "status": outcome.status,
        "output_directory_name": outcome.output_dir.name,
        "scene_digest": loaded.scene_digest,
        "blocks_total": outcome.total_blocks,
        "blocks_completed": outcome.completed_blocks,
        "blocks_reused": outcome.reused_blocks,
        "blocks_recomputed": outcome.recomputed_blocks,
    }
    if outcome.recovery is not None:
        payload["recovery_status"] = outcome.recovery["status"]
    _print_json(payload)
    return 0


def _command_make_scene(args: argparse.Namespace) -> int:
    scene = write_sample_scene(
        args.out,
        image_width=args.width,
        image_height=args.height,
        chunk_width=args.chunk_width,
        chunk_height=args.chunk_height,
    )
    _print_json(
        {
            "ok": True,
            "scene_name": scene["scene_name"],
            "output_file": args.out.name,
            "layers": len(scene["layers"]),
        }
    )
    return 0


def _command_compare(args: argparse.Namespace) -> int:
    ok, report = compare_outputs(
        args.reference,
        args.candidate,
        args.report,
    )
    _print_json(report)
    return 0 if ok else 1


def _command_verify_resume(args: argparse.Namespace) -> int:
    loaded = load_scene(args.scene)
    full_dir = args.out / "full"
    resumed_dir = args.out / "resumed"
    run_render(loaded, full_dir, restart=True)
    interrupted = run_render(
        loaded,
        resumed_dir,
        restart=True,
        interrupt_after=args.interrupt_after,
    )
    resumed = run_render(loaded, resumed_dir)
    report_path = args.report or (args.out / "verification.json")
    ok, report = compare_outputs(full_dir, resumed_dir, report_path)
    payload = {
        "ok": ok,
        "interrupted_status": interrupted.status,
        "resumed_status": resumed.status,
        "blocks_reused": resumed.reused_blocks,
        "blocks_recomputed": resumed.recomputed_blocks,
        "verification_report": report_path.name,
        "difference_count": report["difference_count"],
        "pixel_difference_count": report["pixel_difference_count"],
    }
    _print_json(payload)
    return 0 if ok else 1


def _print_json(value: Any, stream: Any = None) -> None:
    stream = stream or sys.stdout
    stream.write(
        json.dumps(
            value,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        )
        + "\n"
    )
