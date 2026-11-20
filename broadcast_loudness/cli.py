"""Command line entry points for synthesis, offline analysis, demo, and GUI."""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from .analyzer import analyze_audio
from .demo import load_operations, run_demo
from .errors import LoudnessError
from .params import AnalysisParameters, downmix_51_to_stereo
from .report import load_report, save_report_document
from .synth import generate_sample_file
from .wavio import read_wav


def _add_parameter_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--target-lufs", type=float, default=-23.0)
    parser.add_argument("--true-peak-limit", type=float, default=-1.0)
    parser.add_argument("--absolute-gate", type=float, default=-70.0)
    parser.add_argument("--relative-gate", type=float, default=-10.0)
    parser.add_argument(
        "--downmix",
        choices=("identity", "stereo"),
        default="identity",
    )
    parser.add_argument(
        "--weights",
        help="comma-separated loudness weights for the downmix output",
    )


def _params_from_args(audio: Any, args: argparse.Namespace) -> AnalysisParameters:
    if args.downmix == "stereo":
        if audio.channels != 6:
            raise ValueError("--downmix stereo requires a 5.1 input")
        base = AnalysisParameters.for_channels(
            audio.channels,
            downmix=downmix_51_to_stereo(),
            weights=(1.0, 1.0),
            output_layout=("L", "R"),
        )
    else:
        base = AnalysisParameters.for_channels(audio.channels)
    weights = (
        tuple(float(value) for value in args.weights.split(","))
        if args.weights
        else base.weights
    )
    return base.with_updates(
        channel_count=audio.channels,
        target_lufs=args.target_lufs,
        true_peak_limit_dbtp=args.true_peak_limit,
        absolute_gate_lufs=args.absolute_gate,
        relative_gate_lu=args.relative_gate,
        weights=weights,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="bl-meter")
    subparsers = parser.add_subparsers(dest="command", required=True)

    synth = subparsers.add_parser("synth", help="generate deterministic WAV material")
    synth.add_argument("--seed", type=int, required=True)
    synth.add_argument("--out", required=True)
    synth.add_argument("--channels", type=int, choices=range(1, 7), default=6)
    synth.add_argument("--duration", type=float, default=12.0)
    synth.add_argument("--bit-depth", type=int, choices=(16, 24), default=24)

    offline = subparsers.add_parser(
        "offline", help="analyze an entire WAV and write every observation block"
    )
    offline.add_argument("--input", required=True)
    offline.add_argument("--out", required=True)
    _add_parameter_args(offline)

    demo = subparsers.add_parser(
        "demo", help="run deterministic scripted online and offline analysis"
    )
    demo.add_argument("--seed", type=int, required=True)
    demo.add_argument("--operations")
    demo.add_argument("--out-dir", required=True)
    demo.add_argument("--channels", type=int, choices=range(1, 7), default=6)
    demo.add_argument("--duration", type=float, default=12.0)
    demo.add_argument("--bit-depth", type=int, choices=(16, 24), default=24)

    gui = subparsers.add_parser("gui", help="launch the PySide6 loudness meter")
    gui.add_argument("--input")
    gui.add_argument("--refresh-ms", type=int, default=100)

    verify = subparsers.add_parser(
        "verify-report", help="validate report version and hash chain"
    )
    verify.add_argument("--report", required=True)
    verify.add_argument("--input")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "synth":
            manifest = generate_sample_file(
                args.out,
                seed=args.seed,
                channels=args.channels,
                duration_seconds=args.duration,
                bit_depth=args.bit_depth,
            )
            print(
                json.dumps(
                    {
                        "wav": str(Path(args.out).resolve()),
                        "manifest": str(Path(manifest["_manifest_path"]).resolve()),
                        "sha256": manifest["sha256"],
                        "reference": manifest["reference"],
                    },
                    sort_keys=True,
                    ensure_ascii=False,
                )
            )
            return 0
        if args.command == "offline":
            audio = read_wav(args.input)
            params = _params_from_args(audio, args)
            result = analyze_audio(audio, params)
            document = {
                "schema": "broadcast-loudness-offline-baseline",
                "report_version": 1,
                "source": {
                    "filename": Path(audio.filename).name,
                    "sha256": audio.source_sha256,
                    "sample_rate": audio.sample_rate,
                    "bit_depth": audio.bit_depth,
                    "channels": audio.channels,
                    "layout": list(audio.layout),
                    "frames": audio.frame_count,
                },
                "parameters": params.as_dict(),
                "summary": {
                    "integrated_lufs": result.integrated_lufs,
                    "block_count": len(result.blocks),
                },
                "blocks": [block.as_dict() for block in result.blocks],
            }
            digest = save_report_document(args.out, document)
            print(
                json.dumps(
                    {
                        "out": str(Path(args.out).resolve()),
                        "sha256": digest,
                        "blocks": len(result.blocks),
                        "integrated_lufs": result.integrated_lufs,
                    },
                    sort_keys=True,
                )
            )
            return 0
        if args.command == "demo":
            operations = load_operations(args.operations)
            result = run_demo(
                seed=args.seed,
                operations_document=operations,
                output_directory=args.out_dir,
                channels=args.channels,
                duration_seconds=args.duration,
                bit_depth=args.bit_depth,
            )
            print(
                json.dumps(
                    result["validation"],
                    sort_keys=True,
                    ensure_ascii=False,
                )
            )
            return 0
        if args.command == "verify-report":
            audio = read_wav(args.input) if args.input else None
            loaded = load_report(args.report, audio=audio)
            print(
                json.dumps(
                    {
                        "valid": True,
                        "blocks": len(loaded.blocks),
                        "chain_head": loaded.chain_head,
                        "source_sha256": loaded.source_sha256,
                    },
                    sort_keys=True,
                )
            )
            return 0
        if args.command == "gui":
            from .ui import run_gui

            return run_gui(
                input_path=args.input,
                refresh_ms=args.refresh_ms,
            )
    except LoudnessError as exc:
        print(json.dumps(exc.as_dict(), sort_keys=True), file=sys.stderr)
        return 2
    except (OSError, TypeError, ValueError) as exc:
        print(
            json.dumps(
                {"code": "E_OPERATION", "message": str(exc)},
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 2
    return 1
