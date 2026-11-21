"""Command-line interface for sample generation, rendering, and recovery."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .errors import AppError, ErrorCode
from .injection import INJECTIONS, load_inputs
from .pipeline import render_preview, write_preview_outputs
from .samplegen import write_sample


def _print_payload(payload: dict) -> None:
    print(
        json.dumps(
            payload,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
        )
    )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="volrender",
        description="Deterministic CPU volumetric preview renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="write a deterministic local scene sample"
    )
    generate.add_argument("--out", default="artifacts/sample")
    generate.add_argument("--width", type=int, default=48)
    generate.add_argument("--height", type=int, default=32)
    generate.add_argument("--sample-budget", type=int, default=12000)
    generate.add_argument("--tile-width", type=int, default=16)
    generate.add_argument("--tile-height", type=int, default=16)
    generate.add_argument("--max-workers", type=int, default=2)

    render = subparsers.add_parser(
        "render", help="render and publish an atomic preview version"
    )
    render.add_argument("--scene", required=True)
    render.add_argument("--config", required=True)
    render.add_argument("--out", required=True)
    render.add_argument(
        "--mode",
        choices=("both", "float64", "fixed"),
        default="both",
    )
    render.add_argument("--inject", choices=INJECTIONS)
    render.add_argument("--no-seam-check", action="store_true")
    render.add_argument("--snapshot-dir")
    render.add_argument("--resume", action="store_true")
    render.add_argument("--stop-after", type=int)
    render.add_argument("--gui", action="store_true")
    return parser


def _run_generate(args: argparse.Namespace) -> int:
    scene_path, config_path = write_sample(
        args.out,
        width=args.width,
        height=args.height,
        sample_budget=args.sample_budget,
        tile_width=args.tile_width,
        tile_height=args.tile_height,
        max_workers=args.max_workers,
    )
    _print_payload(
        {
            "ok": True,
            "scene": str(scene_path),
            "config": str(config_path),
        }
    )
    return 0


def _run_render(args: argparse.Namespace) -> int:
    loaded = load_inputs(
        args.scene,
        args.config,
        injection=args.inject,
    )
    modes = (
        ("float64", "fixed")
        if args.mode == "both"
        else (args.mode,)
    )
    preview = render_preview(
        loaded.scene,
        loaded.config,
        precision_modes=modes,
        verify_seams=not args.no_seam_check,
        snapshot_dir=args.snapshot_dir,
        resume=args.resume,
        stop_after=args.stop_after,
    )
    publication = write_preview_outputs(args.out, preview)
    payload = {
        "ok": True,
        "version_id": publication["version_id"],
        "output_dir": publication["output_dir"],
        "current": publication["current"],
        "precision": list(preview.renders),
        "degraded_tiles": {
            precision: artifact.summary["degraded_tile_indices"]
            for precision, artifact in preview.renders.items()
        },
        "budget_used": {
            precision: artifact.summary["budget"]["used"]
            for precision, artifact in preview.renders.items()
        },
    }
    if preview.precision_error is not None:
        payload["precision_error"] = preview.precision_error
    _print_payload(payload)
    if args.gui:
        from .gui import launch_gui

        launch_gui(preview)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _run_generate(args)
        if args.command == "render":
            return _run_render(args)
        parser.error(f"unsupported command: {args.command}")
        return 2
    except AppError as exc:
        _print_payload(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        _print_payload(
            {
                "ok": False,
                "code": ErrorCode.E_INTERRUPTED.value,
                "message": "render interrupted",
            }
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
