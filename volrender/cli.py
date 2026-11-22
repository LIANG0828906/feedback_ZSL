"""Command line entry point."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .binary import load_scene
from .config import load_config, validate_config
from .errors import BusinessError, ErrorCode
from .fault_injection import INJECTIONS, load_scene_with_injection
from .io_utils import json_bytes, write_render_artifacts
from .samples import generate_samples, verify_samples


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="volrender",
        description="Deterministic offline volumetric light preview renderer",
    )
    parser.add_argument("--version", action="version", version="volrender 1.0.0")
    sub = parser.add_subparsers(dest="command", required=True)

    validate = sub.add_parser("validate", help="validate scene and config inputs")
    validate.add_argument("--scene", required=True)
    validate.add_argument("--config")
    validate.add_argument("--inject", choices=INJECTIONS)

    render = sub.add_parser("render", help="render a scene to a new artifact set")
    render.add_argument("--scene", required=True)
    render.add_argument("--config", required=True)
    render.add_argument("--outdir", required=True)
    render.add_argument("--precision", choices=("float", "fixed"))
    render.add_argument("--compare-precision", action="store_true")
    render.add_argument("--no-seam-check", action="store_true")
    render.add_argument("--inject", choices=INJECTIONS)

    samples = sub.add_parser("samples", help="generate deterministic local samples")
    samples.add_argument("--outdir", required=True)
    samples.add_argument("--seed", type=int, default=20261122)
    samples.add_argument("--no-baselines", action="store_true")
    samples.add_argument("--verify", action="store_true")

    gui = sub.add_parser("gui", help="open the PyQt5 preview")
    gui.add_argument("--scene", required=True)
    gui.add_argument("--config", required=True)
    return parser


def _load_inputs(
    scene_path: str,
    config_path: str | None,
    inject: str | None,
):
    config = None
    if config_path is not None:
        config = load_config(
            config_path,
            inject="config_budget" if inject == "config_budget" else None,
        )
    if inject in (None, "", "config_budget"):
        scene = load_scene(scene_path)
    else:
        scene = load_scene_with_injection(scene_path, inject=inject)
    return scene, config


def _print(value: dict[str, Any], pretty: bool = True) -> None:
    sys.stdout.buffer.write(json_bytes(value, pretty=pretty))
    sys.stdout.buffer.flush()


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "validate":
            scene, config = _load_inputs(args.scene, args.config, args.inject)
            result: dict[str, Any] = {
                "ok": True,
                "scene": {
                    "width": scene.width,
                    "height": scene.height,
                    "media_count": len(scene.media),
                    "density_field_count": len(scene.density_fields),
                    "sample_bytes": scene.source_size,
                },
            }
            if config is not None:
                result["config_hash"] = config.content_hash
            _print(result)
            return 0

        if args.command == "render":
            scene, config = _load_inputs(args.scene, args.config, args.inject)
            assert config is not None
            if args.precision is not None:
                config = config.with_precision(args.precision)
                validate_config(config)
            from .renderer import render_scene

            result = render_scene(
                scene,
                config,
                validate_seams=not args.no_seam_check,
                compare_precision=args.compare_precision,
            )
            manifest = write_render_artifacts(result, args.outdir)
            _print(
                {
                    "ok": True,
                    "outdir": str(Path(args.outdir)),
                    "pixel_hash": result.pixel_hash,
                    "image_hash": result.image_hash,
                    "summary_hash": result.summary["summary_hash"],
                    "seam_check": result.summary["seam_check"],
                    "budget": result.summary["budget"],
                    "degradation": result.summary["degradation"]["counts_by_stage"],
                    "manifest": manifest,
                }
            )
            return 0

        if args.command == "samples":
            if args.verify:
                verification = verify_samples(args.outdir, seed=args.seed)
                _print(verification)
                return 0 if verification["ok"] else 3
            manifest = generate_samples(
                args.outdir,
                seed=args.seed,
                render_baselines=not args.no_baselines,
            )
            _print(
                {
                    "ok": True,
                    "outdir": str(Path(args.outdir)),
                    "manifest_hash": manifest["manifest_hash"],
                    "samples": [
                        {
                            "name": entry["name"],
                            "scene_hash": entry["scene_hash"],
                            "baseline": entry.get("baseline"),
                        }
                        for entry in manifest["samples"]
                    ],
                }
            )
            return 0

        if args.command == "gui":
            scene = load_scene(args.scene)
            config = load_config(args.config)
            from .gui import run_gui

            return run_gui(scene, config)
        raise AssertionError("unhandled command")
    except BusinessError as exc:
        _print(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        _print(
            {
                "ok": False,
                "code": ErrorCode.CANCELLED,
                "message": "interrupted",
            }
        )
        return 130
    except Exception as exc:
        _print(
            {
                "ok": False,
                "code": ErrorCode.INTERNAL,
                "message": str(exc),
                "type": type(exc).__name__,
            }
        )
        return 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
