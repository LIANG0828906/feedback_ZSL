from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .errors import MasterError
from .manifest import load_manifest
from .render import InterruptFlag, render_project
from .synth import create_demo_project, generate_audio_from_manifest, verify_reference_tracks
from .util import atomic_write_json


def _print_json(value: Any, *, stream: Any = sys.stdout) -> None:
    print(
        json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")),
        file=stream,
        flush=True,
    )


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="choir-master",
        description="Deterministic offline choir mastering with resumable block rendering.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    demo = subparsers.add_parser("demo", help="Create deterministic manifest, eight WAVs, and truth data.")
    demo.add_argument("--dir", required=True, help="Output project directory.")
    demo.add_argument("--seed", type=int, default=2532026)
    demo.add_argument("--sample-rate", type=int, default=48_000)
    demo.add_argument("--bit-depth", type=int, choices=(16, 24, 32), default=24)
    demo.add_argument("--duration-samples", type=int, default=384_000)
    demo.add_argument("--target-lufs", type=float, default=-16.0)
    demo.add_argument("--tp-limit", type=float, default=-1.0)
    demo.add_argument("--block-size", type=int, default=8192)
    demo.add_argument("--no-force", action="store_true")

    generate = subparsers.add_parser("generate", help="Generate all declared WAV files from a manifest.")
    generate.add_argument("--manifest", required=True)
    generate.add_argument("--force", action="store_true")

    verify = subparsers.add_parser("verify-references", help="Independently recompute and verify reference channels.")
    verify.add_argument("--manifest", required=True)

    render = subparsers.add_parser("render", help="Analyze, compensate, balance, and render the stereo master.")
    render.add_argument("--manifest", required=True)
    render.add_argument("--output", required=True)
    render.add_argument("--snapshot", required=True)
    render.add_argument("--report", required=True)
    render.add_argument("--recovery-report")
    render.add_argument("--fresh", action="store_true", help="Discard any existing snapshot and restart.")
    return parser


def _error_report_path(args: argparse.Namespace) -> Path | None:
    if args.command == "render":
        return Path(args.report).expanduser().resolve()
    return None


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "demo":
            report = create_demo_project(
                args.dir,
                force=not args.no_force,
                seed=args.seed,
                sample_rate=args.sample_rate,
                bit_depth=args.bit_depth,
                duration_samples=args.duration_samples,
                target_loudness_lufs=args.target_lufs,
                true_peak_limit_dbtp=args.tp_limit,
                block_size=args.block_size,
            )
            _print_json(report)
            return 0

        if args.command == "generate":
            manifest = load_manifest(args.manifest)
            report = generate_audio_from_manifest(manifest, force=args.force)
            _print_json(report)
            return 0

        if args.command == "verify-references":
            report = verify_reference_tracks(args.manifest)
            _print_json(report)
            return 0 if report["all_reference_tracks_match"] else 4

        if args.command == "render":
            with InterruptFlag() as interrupt:
                result = render_project(
                    manifest_path=args.manifest,
                    output_path=args.output,
                    snapshot_path=args.snapshot,
                    report_path=args.report,
                    recovery_report_path=args.recovery_report,
                    fresh=args.fresh,
                    interrupt=interrupt,
                )
            _print_json(result)
            return int(result.get("exit_code", 0))
        parser.error("unknown command")
        return 2
    except MasterError as exc:
        payload = exc.as_dict()
        report_path = _error_report_path(args)
        if report_path is not None:
            atomic_write_json(report_path, payload)
        _print_json(payload, stream=sys.stderr)
        return int(exc.exit_code)
    except KeyboardInterrupt:
        payload = {
            "status": "interrupted",
            "code": "E_INTERRUPTED_UNMANAGED",
            "message": "Interrupted before the renderer could commit a block.",
        }
        _print_json(payload, stream=sys.stderr)
        return 0


if __name__ == "__main__":
    raise SystemExit(main())

