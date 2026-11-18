"""Command-line interface for validation, deterministic scene creation, and rendering."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .errors import StructuredError
from .meshgen import make_scene, write_scene
from .pipeline import render_scene
from .scene import load_scene, validate_scene


def _json_print(payload: dict) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, allow_nan=False))


def _cmd_make_scene(args: argparse.Namespace) -> int:
    document = make_scene(
        width=args.width,
        height=args.height,
        tile_size=args.tile_size,
        plate_gap=args.gap,
        base_seed=args.seed,
        radial_segments=args.segments,
        rings=args.rings,
        texture_resolution=args.texture_resolution,
        projection_samples=args.projection_samples,
    )
    scene = validate_scene(document, source_name=args.out)
    write_scene(args.out, document)
    _json_print(
        {
            "ok": True,
            "scene": args.out,
            "scene_digest": scene.digest,
            "image": {"width": scene.width, "height": scene.height},
            "tile_size": scene.tile_size,
            "plates": len(scene.plates),
            "layers": len(scene.layers),
            "lights": len(scene.lights),
        }
    )
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    _json_print(
        {
            "ok": True,
            "scene": Path(args.scene).name,
            "scene_digest": scene.digest,
            "image": {"width": scene.width, "height": scene.height},
            "tile_size": scene.tile_size,
            "plates": len(scene.plates),
            "layers": len(scene.layers),
            "lights": len(scene.lights),
        }
    )
    return 0


def _cmd_render(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    result = render_scene(
        scene,
        args.out,
        stop_after_tiles=args.stop_after_tiles,
        reset=args.reset,
        verify_resume=not args.no_verify_resume,
        install_signal_handlers=True,
    )
    report = result.report
    _json_print(
        {
            "ok": True,
            "status": result.status,
            "report": "report.json",
            "resume_report": "resume_report.json",
            "image_digest": report["image"]["digest"],
            "depth_digest": report["optical_depth"]["digest"],
            "completed_tiles": report["tiles"]["completed_count"],
            "total_tiles": report["tiles"]["total"],
            "resume_count": report["recovery"]["resume_count"],
        }
    )
    return result.exit_code


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="celadon-museum",
        description="Deterministic offline soft rasterizer for a stacked celadon preview.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    make_parser = subparsers.add_parser("make-scene", help="write the deterministic reference scene")
    make_parser.add_argument("--out", required=True, help="scene JSON destination")
    make_parser.add_argument("--width", type=int, default=384)
    make_parser.add_argument("--height", type=int, default=256)
    make_parser.add_argument("--tile-size", type=int, default=64)
    make_parser.add_argument("--gap", type=float, default=0.19, help="plate-to-plate vertical gap")
    make_parser.add_argument("--seed", type=int, default=20261118)
    make_parser.add_argument("--segments", type=int, default=32)
    make_parser.add_argument("--rings", type=int, default=8)
    make_parser.add_argument("--texture-resolution", type=int, default=128)
    make_parser.add_argument("--projection-samples", type=int, default=12)
    make_parser.set_defaults(func=_cmd_make_scene)

    validate_parser = subparsers.add_parser("validate", help="validate a scene and print its digest")
    validate_parser.add_argument("--scene", required=True)
    validate_parser.set_defaults(func=_cmd_validate)

    render_parser = subparsers.add_parser("render", help="render or resume the tiled preview")
    render_parser.add_argument("--scene", required=True)
    render_parser.add_argument("--out", required=True)
    render_parser.add_argument(
        "--stop-after-tiles",
        type=int,
        default=None,
        help="after N committed tiles, write an interruption report and exit 0",
    )
    render_parser.add_argument("--reset", action="store_true", help="discard all snapshots first")
    render_parser.add_argument(
        "--no-verify-resume",
        action="store_true",
        help="skip the default complete rescan used to prove resumed bytes match",
    )
    render_parser.set_defaults(func=_cmd_render)
    return parser


def main(argv: list[str] | None = None) -> int:
    normalized = list(sys.argv[1:] if argv is None else argv)
    if (
        normalized
        and normalized[0] not in ("make-scene", "validate", "render", "-h", "--help")
        and "--scene" in normalized
    ):
        normalized.insert(0, "render")
    parser = _build_parser()
    args = parser.parse_args(normalized)
    if getattr(args, "stop_after_tiles", None) is not None and args.stop_after_tiles <= 0:
        parser.error("--stop-after-tiles must be positive")
    try:
        return int(args.func(args))
    except StructuredError as exc:
        _json_print({"ok": False, "error": exc.to_dict()})
        return exc.exit_code
    except Exception as exc:  # keep the CLI machine-readable
        _json_print(
            {
                "ok": False,
                "error": {
                    "code": "E_INTERNAL",
                    "plate_id": None,
                    "layer_id": None,
                    "reason": f"{type(exc).__name__}: {exc}",
                },
            }
        )
        return 4
