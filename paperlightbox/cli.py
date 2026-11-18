"""Command line entry point for the paper lightbox renderer."""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys

from .errors import SceneError, render_error_payload
from .pipeline import render_scene
from .report import build_report, compare_outputs, write_report
from .sample_gen import build_gap_variant, build_sample_scene
from .scene import load_scene
import numpy as np


def _write_json(path: str, payload: dict) -> None:
    def _convert(value):
        if isinstance(value, np.ndarray):
            return value.tolist()
        if isinstance(value, dict):
            return {k: _convert(v) for k, v in value.items()}
        if isinstance(value, list):
            return [_convert(v) for v in value]
        return value

    with open(path, "w", encoding="utf-8") as handle:
        json.dump(_convert(payload), handle, sort_keys=True, indent=2)
        handle.write("\n")


def _emit_error(error: SceneError, error_path: str | None = None) -> int:
    payload = render_error_payload(error)
    if error_path:
        with open(error_path, "w", encoding="utf-8") as handle:
            handle.write(payload)
            handle.write("\n")
    sys.stderr.write(payload + "\n")
    return int(error.code) if error.code != 0 else 1


def cmd_generate(args) -> int:
    scene = build_sample_scene(width=args.width, height=args.height,
                               tile=args.tile,
                               sample_density=args.sample_density)
    os.makedirs(os.path.dirname(os.path.abspath(args.scene)),
                exist_ok=True)
    _write_json(args.scene, scene)
    if args.gap_variant:
        variant = build_gap_variant(scene, args.gap_scale)
        os.makedirs(os.path.dirname(os.path.abspath(args.gap_variant)),
                    exist_ok=True)
        _write_json(args.gap_variant, variant)
    return 0


def cmd_render(args) -> int:
    try:
        scene = load_scene(args.scene)
        stop_file = args.stop_file

        def _handle_signal(signum, frame):  # pragma: no cover - signal path
            if stop_file:
                with open(stop_file, "w", encoding="utf-8") as handle:
                    handle.write("stop\n")

        previous = {sig: signal.getsignal(sig) for sig in (
            signal.SIGTERM, signal.SIGINT) if hasattr(signal, "SIGTERM")}
        for sig in previous:
            signal.signal(sig, _handle_signal)
        try:
            summary = render_scene(scene, args.output, args.snapshot,
                                   stop_file=stop_file)
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)

        if summary["stopped_gracefully"]:
            recovery_report = {
                "status": "interrupted",
                "completed_tiles": summary["completed"],
                "total_tiles": summary["total_tiles"],
                "scene_digest": summary["store"].digest,
                "recovery_count": summary["recovery_count"],
            }
            _write_json(os.path.join(args.output, "recovery_report.json"),
                        recovery_report)
            return 0

        report = build_report(scene, summary)
        write_report(report, os.path.join(args.output, "report.json"))
        return 0
    except SceneError as exc:
        return _emit_error(exc, args.error_json)


def cmd_compare(args) -> int:
    scene = load_scene(args.scene)
    result = compare_outputs(
        args.reference_png, args.candidate_png,
        args.reference_depth, args.candidate_depth,
        int(scene["render"]["tile_width"]),
        int(scene["render"]["tile_height"]))
    _write_json(args.diff_report, result)
    return 0 if result["status"] == "identical" else 12


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="paperlightbox",
        description="Deterministic offline paper-cut lightbox rasterizer")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="build deterministic sample scenes")
    gen.add_argument("--scene", required=True)
    gen.add_argument("--gap-variant", default=None)
    gen.add_argument("--gap-scale", type=float, default=2.0)
    gen.add_argument("--width", type=int, default=320)
    gen.add_argument("--height", type=int, default=320)
    gen.add_argument("--tile", type=int, default=64)
    gen.add_argument("--sample-density", type=int, default=9)
    gen.set_defaults(func=cmd_generate)

    render = sub.add_parser("render", help="render a scene with resume support")
    render.add_argument("--scene", required=True)
    render.add_argument("--output", required=True)
    render.add_argument("--snapshot", required=True)
    render.add_argument("--stop-file", default=None)
    render.add_argument("--error-json", default=None)
    render.set_defaults(func=cmd_render)

    compare = sub.add_parser("compare", help="verify byte-identical reruns")
    compare.add_argument("--scene", required=True)
    compare.add_argument("--reference-png", required=True)
    compare.add_argument("--candidate-png", required=True)
    compare.add_argument("--reference-depth", required=True)
    compare.add_argument("--candidate-depth", required=True)
    compare.add_argument("--diff-report", required=True)
    compare.set_defaults(func=cmd_compare)
    return parser


def main(argv: list | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
