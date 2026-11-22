from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Sequence

from .errors import ErrorCode, WorkbenchError
from .export import save_rgba_png
from .generator import DEFAULT_SEED, generate_samples
from .model import RenderParams
from .operations import (
    load_sample_scene,
    run_acceptance,
    validate_flattening,
)
from .pipeline import Pipeline
from .validation import load_scene


def _add_render_parameters(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--scale", type=float, default=1.0)
    parser.add_argument("--tolerance", type=float, default=0.25)
    parser.add_argument("--subsamples", type=int, default=4)
    parser.add_argument("--workers", type=int, default=2)
    parser.add_argument("--buffer-mb", type=float, default=32.0)
    parser.add_argument("--max-stroke-pixels", type=float, default=4096.0)


def _params(args: argparse.Namespace) -> RenderParams:
    return RenderParams(
        scale=args.scale,
        tolerance=args.tolerance,
        subsamples=args.subsamples,
        max_parallelism=args.workers,
        coverage_buffer_limit=max(1, int(args.buffer_mb * 1024 * 1024)),
        max_stroke_pixels=args.max_stroke_pixels,
    )


def _load_scene_argument(args: argparse.Namespace):
    if args.scene:
        return load_scene(args.scene, args.truth)
    if not args.scene_id:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "either --scene or --scene-id is required",
            field="scene",
        )
    return load_sample_scene(args.data, args.scene_id)


def _command_generate(args: argparse.Namespace) -> int:
    manifest = generate_samples(args.out, args.seed)
    print(
        f"seed={args.seed} scenes={len(manifest['scenes'])} out={Path(args.out).resolve()}"
    )
    return 0


def _command_validate(args: argparse.Namespace) -> int:
    scene = _load_scene_argument(args)
    report = validate_flattening(scene, _params(args))
    payload = {
        "scene_id": scene.scene_id,
        "paths": len(scene.paths),
        "render_bounds": list(scene.render_bounds),
        "flattening": report,
    }
    print(json.dumps(payload, indent=2, ensure_ascii=False))
    return 0 if report["passed"] else 1


def _command_render(args: argparse.Namespace) -> int:
    scene = _load_scene_argument(args)
    params = _params(args)
    root = Path(args.checkpoints) / scene.scene_id
    pipeline = Pipeline(scene, params, root)
    result = pipeline.run(args.mode, raise_on_error=False)
    if result.status != "completed":
        print(
            json.dumps(
                {
                    "status": result.status,
                    "recoverable": result.recoverable,
                    "errors": result.errors,
                    "progress": [
                        result.progress_committed,
                        result.progress_total,
                    ],
                },
                indent=2,
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 2
    output = save_rgba_png(pipeline.canvas, args.out)
    print(
        json.dumps(
            {
                "status": result.status,
                "scene_id": scene.scene_id,
                "size": list(pipeline.image_size),
                "progress": [result.progress_committed, result.progress_total],
                "commit_sequence": result.commit_sequence,
                "output": str(output.resolve()),
            },
            indent=2,
            ensure_ascii=False,
        )
    )
    return 0


def _command_verify(args: argparse.Namespace) -> int:
    data = Path(args.data)
    if not (data / "samples.json").exists():
        generate_samples(data, args.seed)
    report = run_acceptance(
        data,
        args.artifacts,
        args.checkpoints,
        seed=args.seed,
        params=RenderParams(),
    )
    summary = {
        "passed": report["passed"],
        "seed": report["seed"],
        "scenes": [
            {
                "scene_id": scene["scene_id"],
                "dimensions": scene["dimensions"],
                "flattening": scene["flattening"]["passed"],
                "three_paths": scene["three_paths"]["passed"],
                "image_sha256": scene["three_paths"]["full"]["image_sha256"],
            }
            for scene in report["scenes"]
        ],
        "anomaly_injection": report["anomaly_injection"]["passed"],
        "snapshot_invalidation": report["snapshot_invalidation"]["passed"],
        "diff_detection": report["diff_detection"]["passed"],
        "report": str((Path(args.artifacts) / "validation_report.json").resolve()),
    }
    print(json.dumps(summary, indent=2, ensure_ascii=False))
    return 0 if report["passed"] else 1


def _command_gui(args: argparse.Namespace) -> int:
    if args.offscreen:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    from .ui import run_gui

    return run_gui(Path(args.data), Path(args.checkpoints), args.scene_id)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="vector-workbench",
        description="Deterministic vector path compositing and diagnostics workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out", default="data")
    generate.add_argument("--seed", type=int, default=DEFAULT_SEED)
    generate.set_defaults(handler=_command_generate)

    validate = subparsers.add_parser("validate", help="validate a scene and flattening truth")
    validate.add_argument("--scene")
    validate.add_argument("--truth")
    validate.add_argument("--data", default="data")
    validate.add_argument("--scene-id")
    _add_render_parameters(validate)
    validate.set_defaults(handler=_command_validate)

    render = subparsers.add_parser("render", help="render a scene to PNG")
    render.add_argument("--scene")
    render.add_argument("--truth")
    render.add_argument("--data", default="data")
    render.add_argument("--scene-id")
    render.add_argument("--checkpoints", default="var/checkpoints/render")
    render.add_argument("--mode", choices=("full", "incremental", "resume"), default="full")
    render.add_argument("--out", default="artifacts/render.png")
    _add_render_parameters(render)
    render.set_defaults(handler=_command_render)

    verify = subparsers.add_parser(
        "verify", help="run the deterministic acceptance suite"
    )
    verify.add_argument("--data", default="data")
    verify.add_argument("--artifacts", default="artifacts")
    verify.add_argument("--checkpoints", default="var/checkpoints/acceptance")
    verify.add_argument("--seed", type=int, default=DEFAULT_SEED)
    verify.set_defaults(handler=_command_verify)

    gui = subparsers.add_parser("gui", help="launch the diagnostics desktop workbench")
    gui.add_argument("--data", default="data")
    gui.add_argument("--checkpoints", default="var/checkpoints/gui")
    gui.add_argument("--scene-id")
    gui.add_argument("--offscreen", action="store_true")
    gui.set_defaults(handler=_command_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except WorkbenchError as exc:
        print(json.dumps(exc.to_dict(), indent=2, ensure_ascii=False), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

