from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .generator import generate_project
from .pipeline import process_project
from .verify import verify_master


def _cmd_generate(args: argparse.Namespace) -> int:
    target = None if args.no_loudness_target else args.target_loudness
    manifest = generate_project(
        args.out_dir,
        duration=args.duration,
        seed=args.seed,
        name=args.name,
        output_sample_rate=args.sample_rate,
        output_channels=args.channels,
        output_bits_per_sample=args.bits,
        target_loudness_lufs=target,
    )
    print(json.dumps({"manifest": str(manifest), "status": "generated"}, sort_keys=True))
    return 0


def _cmd_process(args: argparse.Namespace) -> int:
    manifest = Path(args.manifest)
    master = Path(args.master) if args.master else manifest.parent / "master.wav"
    report = Path(args.report) if args.report else manifest.parent / "report.json"
    result = process_project(manifest, master, report)
    summary = {
        "master": str(result.master_path),
        "report": str(result.report_path),
        "status": result.status,
    }
    if not result.ok:
        summary["hard_error_codes"] = [
            issue["code"] for issue in result.report.get("hard_errors", [])
        ]
        summary["quality_warning_codes"] = [
            issue["code"] for issue in result.report.get("quality_warnings", [])
        ]
    print(json.dumps(summary, sort_keys=True))
    return 0 if result.ok else 2


def _cmd_demo(args: argparse.Namespace) -> int:
    out_dir = Path(args.out_dir)
    target = None if args.no_loudness_target else args.target_loudness
    manifest = generate_project(
        out_dir,
        duration=args.duration,
        seed=args.seed,
        name=args.name,
        output_sample_rate=args.sample_rate,
        output_channels=args.channels,
        output_bits_per_sample=args.bits,
        target_loudness_lufs=target,
    )
    result = process_project(manifest, out_dir / "master.wav", out_dir / "report.json")
    summary = {
        "manifest": str(manifest),
        "master": str(result.master_path),
        "report": str(result.report_path),
        "status": result.status,
    }
    print(json.dumps(summary, sort_keys=True))
    return 0 if result.ok else 2


def _cmd_verify(args: argparse.Namespace) -> int:
    result = verify_master(args.manifest, args.master)
    print(json.dumps(result, sort_keys=True))
    return 0 if result["status"] == "ok" else 2


def _add_generation_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--duration", type=float, default=180.0)
    parser.add_argument("--seed", type=int, default=20261118)
    parser.add_argument("--name", default="deterministic_indie_band")
    parser.add_argument("--sample-rate", type=int, default=48000)
    parser.add_argument("--channels", type=int, default=2)
    parser.add_argument("--bits", type=int, choices=(16, 24, 32), default=24)
    parser.add_argument("--target-loudness", type=float, default=-16.0)
    parser.add_argument("--no-loudness-target", action="store_true")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="offline-mastering",
        description="Deterministic offline multitrack mastering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate a deterministic project")
    _add_generation_arguments(generate)
    generate.set_defaults(func=_cmd_generate)

    process = subparsers.add_parser("process", help="process an existing manifest")
    process.add_argument("--manifest", required=True)
    process.add_argument("--master")
    process.add_argument("--report")
    process.set_defaults(func=_cmd_process)

    demo = subparsers.add_parser("demo", help="generate and process a complete demo")
    _add_generation_arguments(demo)
    demo.set_defaults(func=_cmd_demo)

    verify = subparsers.add_parser("verify", help="independently verify a master WAV")
    verify.add_argument("--manifest", required=True)
    verify.add_argument("--master", required=True)
    verify.set_defaults(func=_cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    sys.exit(main())
