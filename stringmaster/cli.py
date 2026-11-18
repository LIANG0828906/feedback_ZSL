"""Command line interface for deterministic generation and mastering."""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys
from typing import Any

from .audio import atomic_write_json
from .errors import BusinessError, ManifestError
from .manifest import load_manifest
from .render import render_project
from .synth import generate_project


def _demo_manifest(duration: float, seed: int) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "seed": seed,
        "sample_rate": 48000,
        "bit_depth": 16,
        "duration_seconds": duration,
        "block_size": 8192,
        "crossover_band": {"low_hz": 2500.0, "high_hz": 5200.0},
        "target_loudness_lufs": -20.0,
        "true_peak_limit_dbtp": -1.0,
        "tracks": [
            {
                "path": "tracks/violin_1.wav",
                "instrument": "第一小提琴",
                "distance_m": 0.65,
                "azimuth_deg": -28.0,
                "target_energy_ratio": 0.27,
                "downmix_weights": [1.0, 0.82],
            },
            {
                "path": "tracks/violin_2.wav",
                "instrument": "第二小提琴",
                "distance_m": 1.15,
                "azimuth_deg": 23.0,
                "target_energy_ratio": 0.24,
                "downmix_weights": [0.84, 1.0],
            },
            {
                "path": "tracks/viola.wav",
                "instrument": "中提琴",
                "distance_m": 1.75,
                "azimuth_deg": 36.0,
                "target_energy_ratio": 0.24,
                "downmix_weights": [0.92, 0.92],
            },
            {
                "path": "tracks/cello.wav",
                "instrument": "大提琴",
                "distance_m": 2.35,
                "azimuth_deg": -19.0,
                "target_energy_ratio": 0.25,
                "downmix_weights": [0.90, 0.90],
            },
        ],
    }


def create_demo(root: str, *, duration: float, seed: int) -> dict[str, Any]:
    root = os.path.abspath(root)
    os.makedirs(os.path.join(root, "tracks"), exist_ok=True)
    manifest_path = os.path.join(root, "manifest.json")
    atomic_write_json(manifest_path, _demo_manifest(duration, seed))
    context = load_manifest(manifest_path, require_files=False)
    truth = generate_project(context)
    truth["manifest_path"] = manifest_path
    atomic_write_json(os.path.join(root, "score_truth.json"), truth)
    return truth


def _error_payload(exc: BusinessError) -> dict[str, Any]:
    if isinstance(exc, ManifestError):
        return {"errors": [issue.as_dict() for issue in exc.issues]}
    return {"errors": [exc.as_dict()]}


def _exit_code(exc: BusinessError) -> int:
    if exc.code == "E_RESUME_MISMATCH":
        return 3
    if exc.code.startswith("E_MANIFEST") or exc.code in {
        "E_FILE_MISSING",
        "E_FILE_READ",
        "E_FILE_CHANGED",
        "E_WAV_FORMAT",
        "E_WAV_CORRUPT",
        "E_SAMPLE_RATE_MISMATCH",
        "E_BIT_DEPTH_MISMATCH",
        "E_CHANNEL_MISMATCH",
        "E_CHANNEL_UNSUPPORTED",
        "E_INSTRUMENT_MISSING",
        "E_INSTRUMENT_DUPLICATE",
        "E_RATIO_SUM",
        "E_BAND_INVALID",
    }:
        return 2
    return 4


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="stringmaster",
        description="Deterministic string-quartet transition repair and mastering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    demo = subparsers.add_parser("demo", help="create a deterministic demo project")
    demo.add_argument("--root", required=True)
    demo.add_argument("--duration", type=float, default=8.0)
    demo.add_argument("--seed", type=int, default=20261118)

    generate = subparsers.add_parser(
        "generate", help="synthesize the four WAV files declared by a manifest"
    )
    generate.add_argument("--manifest", required=True)

    validate = subparsers.add_parser(
        "validate", help="validate manifest and declared WAV files"
    )
    validate.add_argument("--manifest", required=True)

    render = subparsers.add_parser(
        "render", help="run detection, compensation, balance, and resumable mastering"
    )
    render.add_argument("--manifest", required=True)
    render.add_argument("--output")
    render.add_argument("--state")
    render.add_argument("--report")
    render.add_argument("--recovery-report")
    render.add_argument("--block-size", type=int)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.command == "demo":
            result = create_demo(args.root, duration=args.duration, seed=args.seed)
            print(json.dumps(result, ensure_ascii=False, sort_keys=True))
            return 0
        if args.command == "generate":
            context = load_manifest(args.manifest, require_files=False)
            result = generate_project(context)
            print(json.dumps(result, ensure_ascii=False, sort_keys=True))
            return 0
        if args.command == "validate":
            context = load_manifest(args.manifest, require_files=True)
            result = {
                "status": "valid",
                "manifest": context.manifest_path,
                "manifest_digest": context.digest,
                "instruments": [track.instrument for track in context.tracks],
                "frames": [track.wav.frames for track in context.tracks],
            }
            print(json.dumps(result, ensure_ascii=False, sort_keys=True))
            return 0
        if args.command == "render":
            interrupted = {"requested": False}

            def handle_interrupt(_signum: int, _frame: Any) -> None:
                interrupted["requested"] = True

            previous = signal.signal(signal.SIGINT, handle_interrupt)
            try:
                result = render_project(
                    args.manifest,
                    output_path=args.output,
                    state_path=args.state,
                    report_path=args.report,
                    recovery_report_path=args.recovery_report,
                    block_size=args.block_size,
                    interrupt_requested=lambda: interrupted["requested"],
                )
            finally:
                signal.signal(signal.SIGINT, previous)
            print(json.dumps(result, ensure_ascii=False, sort_keys=True))
            return 0
    except BusinessError as exc:
        print(
            json.dumps(_error_payload(exc), ensure_ascii=False, sort_keys=True),
            file=sys.stderr,
        )
        return _exit_code(exc)
    except KeyboardInterrupt:
        return 130
    return 1
