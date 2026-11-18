"""Command line entry points."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from . import __version__
from .compare import compare_runs
from .errors import ErrorCode, SceneValidationError
from .runner import run_render
from .scene import load_scene
from .scene_builder import build_scene
from .util import atomic_write_json


def _emit_error(code: str, message: str, **context: Any) -> None:
    record = {
        "severity": "error",
        "code": code,
        "layer": context.get("layer"),
        "leaf": context.get("leaf"),
        "message": message,
    }
    if context:
        record["context"] = context
    print(json.dumps(record, ensure_ascii=False, sort_keys=True), file=sys.stderr)


def _cmd_build_scene(args: argparse.Namespace) -> int:
    scene = build_scene()
    output = Path(args.out)
    output.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(
        scene, ensure_ascii=False, sort_keys=True, indent=2, allow_nan=False
    ).encode("utf-8") + b"\n"
    output.write_bytes(data)
    print(
        json.dumps(
            {
                "status": "ok",
                "scene": str(output),
                "bytes": len(data),
                "leaves": len(scene["leaves"]),
            },
            ensure_ascii=False,
            sort_keys=True,
        )
    )
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    try:
        scene = load_scene(args.scene)
    except SceneValidationError as exc:
        for issue in exc.as_dicts():
            print(
                json.dumps(issue, ensure_ascii=False, sort_keys=True),
                file=sys.stderr,
            )
        return exc.exit_code
    print(
        json.dumps(
            {
                "status": "ok",
                "scene_id": scene.raw.get("scene_id"),
                "scene_digest": scene.digest,
                "layers": len(scene.layers),
                "leaves": len(scene.leaves),
                "lights": len(scene.lights),
                "angles_deg": list(scene.camera.angles_deg),
            },
            ensure_ascii=False,
            sort_keys=True,
        )
    )
    return 0


def _cmd_render(args: argparse.Namespace) -> int:
    try:
        result = run_render(
            args.scene,
            args.out,
            force_fresh=args.fresh,
            stop_after_blocks=args.stop_after_blocks,
            quiet=args.quiet,
        )
    except SceneValidationError as exc:
        for issue in exc.as_dicts():
            print(
                json.dumps(issue, ensure_ascii=False, sort_keys=True),
                file=sys.stderr,
            )
        return exc.exit_code
    except Exception as exc:
        _emit_error(ErrorCode.IO_FAILURE, f"{type(exc).__name__}: {exc}")
        return 3
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return 0


def _cmd_compare(args: argparse.Namespace) -> int:
    try:
        result = compare_runs(args.reference, args.candidate)
    except Exception as exc:
        _emit_error(
            ErrorCode.RESUME_MISMATCH,
            f"comparison failed: {type(exc).__name__}: {exc}",
        )
        return 4
    if args.json:
        atomic_write_json(args.json, result)
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return int(result["exit_code"])


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m gallery_louvers",
        description="Deterministic perforated aluminium louver renderer",
    )
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    build = subparsers.add_parser(
        "build-scene", help="write the deterministic local sample scene JSON"
    )
    build.add_argument("--out", required=True, help="output scene JSON path")
    build.set_defaults(func=_cmd_build_scene)

    validate = subparsers.add_parser("validate", help="validate a scene JSON")
    validate.add_argument("scene")
    validate.set_defaults(func=_cmd_validate)

    render = subparsers.add_parser(
        "render", help="tile-render all camera samples, resuming from snapshot"
    )
    render.add_argument("scene")
    render.add_argument("--out", required=True, help="run/work directory")
    render.add_argument(
        "--fresh",
        action="store_true",
        help="discard committed progress and start at angle 0/block 0",
    )
    render.add_argument(
        "--stop-after-blocks",
        type=int,
        default=None,
        help="deterministic graceful stop hook after N new blocks (tests)",
    )
    render.add_argument("--quiet", action="store_true")
    render.set_defaults(func=_cmd_render)

    compare = subparsers.add_parser(
        "compare", help="byte-compare two completed render directories"
    )
    compare.add_argument("reference")
    compare.add_argument("candidate")
    compare.add_argument("--json", default=None, help="optional diff JSON path")
    compare.set_defaults(func=_cmd_compare)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))
