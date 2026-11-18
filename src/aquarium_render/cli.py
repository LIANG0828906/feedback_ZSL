"""Command line interface for the deterministic aquarium renderer."""

from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
from pathlib import Path
from typing import Any

from .canonical import canonical_text
from .errors import (
    ERR_IO,
    ERR_SNAPSHOT,
    BusinessError,
    RenderMismatchError,
    SceneValidationError,
)
from .particles import build_particle_table, particle_tables_fingerprint
from .pipeline import run_pipeline
from .scene import build_sample_scene, load_scene, write_scene
from .snapshot import SnapshotResetRequired, load_snapshot
from .verify import compare_run_directories


def _print_json(value: Any, *, stream: Any = sys.stdout) -> None:
    stream.write(canonical_text(value))


def _command_sample(args: argparse.Namespace) -> int:
    scene = build_sample_scene()
    if args.width is not None:
        scene["render"]["width"] = args.width
    if args.height is not None:
        scene["render"]["height"] = args.height
    if args.block_width is not None:
        scene["render"]["block_width"] = args.block_width
    if args.block_height is not None:
        scene["render"]["block_height"] = args.block_height
    if args.angles is not None:
        if args.angles < 1:
            raise ValueError("--angles must be positive")
        scene["camera"]["sample_count"] = args.angles
    digest = write_scene(scene, args.out)
    _print_json(
        {
            "status": "ok",
            "scene": str(Path(args.out)),
            "scene_sha256": digest,
            "width": scene["render"]["width"],
            "height": scene["render"]["height"],
            "angles": scene["camera"]["sample_count"],
        }
    )
    return 0


def _command_validate(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    _print_json(
        {
            "status": "ok",
            "scene_id": scene.scene_id,
            "scene_sha256": scene.summary,
            "glass_layers": len(scene.glass),
            "water_layers": len(scene.water),
            "fish": len(scene.fish),
            "lights": len(scene.lights),
            "angles": len(scene.camera.angles()),
        }
    )
    return 0


def _command_render(args: argparse.Namespace) -> int:
    interrupt = threading.Event()

    def handle_interrupt(signum: int, frame: Any) -> None:
        interrupt.set()

    previous_handler = signal.getsignal(signal.SIGINT)
    signal.signal(signal.SIGINT, handle_interrupt)
    try:
        scene = load_scene(args.scene)
        result = run_pipeline(
            scene,
            args.out,
            interrupt_requested=interrupt.is_set,
            reference_dir=args.reference,
        )
    finally:
        signal.signal(signal.SIGINT, previous_handler)
    payload = {
        "status": result.status,
        "exit_code": result.exit_code,
        "output_dir": str(result.output_dir),
        "snapshot": {
            "completed_blocks": len(result.snapshot.completed),
            "total_blocks": result.snapshot.total_angles * result.snapshot.blocks_per_angle,
            "complete": result.snapshot.complete,
            "resumed_from_blocks": result.snapshot.resumed_from_blocks,
            "reset_reason": result.snapshot.reset_reason,
        },
    }
    if result.status == "mismatch":
        payload["code"] = "AQ-RESUME-001"
        payload["diffs"] = result.diffs
        _print_json(payload, stream=sys.stderr)
    else:
        _print_json(payload)
    return result.exit_code


def _command_compare(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    diffs = compare_run_directories(args.reference, args.actual, scene)
    if diffs:
        payload = {
            "status": "error",
            "code": "AQ-RESUME-001",
            "reference": str(Path(args.reference)),
            "actual": str(Path(args.actual)),
            "diffs": diffs,
        }
        _print_json(payload, stream=sys.stderr)
        if args.output:
            Path(args.output).write_text(canonical_text(payload), encoding="utf-8")
        return 1
    _print_json(
        {
            "status": "ok",
            "message": "RGBA, ownership maps and projection coordinates are byte-identical",
        }
    )
    return 0


def _command_snapshot(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    tables = {
        component.component_id: build_particle_table(
            component.particle_seed,
            component.component_id,
            component.particle_resolution,
        )
        for component in scene.water
    }
    fingerprint = particle_tables_fingerprint(tables)
    try:
        state = load_snapshot(args.snapshot, scene.summary_record, fingerprint)
    except SnapshotResetRequired as exc:
        payload = {
            "status": "error",
            "code": ERR_SNAPSHOT,
            "errors": [
                BusinessError(
                    ERR_SNAPSHOT,
                    "snapshot",
                    "snapshot is stale or corrupt and would be discarded",
                    str(Path(args.snapshot)),
                    {"reason": str(exc)},
                ).to_dict()
            ],
        }
        _print_json(payload, stream=sys.stderr)
        return 2
    _print_json(
        {
            "status": "ok",
            "completed_blocks": len(state.completed),
            "total_blocks": state.total_angles * state.blocks_per_angle,
            "complete": state.complete,
            "next_sequence": state.expected_sequence(),
            "layer_stats": state.layer_stats,
        }
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="aquarium-render",
        description="Deterministic layered glass-aquarium reference renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample = subparsers.add_parser("sample", help="write the deterministic local sample scene")
    sample.add_argument("--out", required=True, type=Path)
    sample.add_argument("--width", type=int)
    sample.add_argument("--height", type=int)
    sample.add_argument("--block-width", type=int)
    sample.add_argument("--block-height", type=int)
    sample.add_argument("--angles", type=int)
    sample.set_defaults(handler=_command_sample)

    validate = subparsers.add_parser("validate", help="validate a scene without rendering")
    validate.add_argument("--scene", required=True, type=Path)
    validate.set_defaults(handler=_command_validate)

    render = subparsers.add_parser("render", help="render and resume all camera angles")
    render.add_argument("--scene", required=True, type=Path)
    render.add_argument("--out", required=True, type=Path)
    render.add_argument(
        "--reference",
        type=Path,
        help="uninterrupted run directory; resumed output is compared pixel-by-pixel",
    )
    render.set_defaults(handler=_command_render)

    compare = subparsers.add_parser("compare", help="compare two completed run directories")
    compare.add_argument("--scene", required=True, type=Path)
    compare.add_argument("--reference", required=True, type=Path)
    compare.add_argument("--actual", required=True, type=Path)
    compare.add_argument("--output", type=Path)
    compare.set_defaults(handler=_command_compare)

    snapshot = subparsers.add_parser("snapshot", help="inspect a progress snapshot")
    snapshot.add_argument("--scene", required=True, type=Path)
    snapshot.add_argument("--snapshot", required=True, type=Path)
    snapshot.set_defaults(handler=_command_snapshot)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except SceneValidationError as exc:
        _print_json(exc.payload(), stream=sys.stderr)
        return 2
    except RenderMismatchError as exc:
        _print_json(exc.payload(), stream=sys.stderr)
        return 1
    except OSError as exc:
        _print_json(
            {
                "status": "error",
                "code": ERR_IO,
                "errors": [
                    BusinessError(
                        ERR_IO, "io", str(exc), "", {"errno": getattr(exc, "errno", None)}
                    ).to_dict()
                ],
            },
            stream=sys.stderr,
        )
        return 3
    except ValueError as exc:
        _print_json(
            {
                "status": "error",
                "code": "AQ-ARG-001",
                "errors": [
                    BusinessError("AQ-ARG-001", "arguments", str(exc), "").to_dict()
                ],
            },
            stream=sys.stderr,
        )
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

