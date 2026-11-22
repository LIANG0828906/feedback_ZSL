"""Command-line entry points."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .canonical import atomic_write_json, canonical_json
from .errors import LCFTError
from .generator import generate_sample
from .loader import load_bundle
from .params import LayoutParams
from .recovery import resume_from_snapshot
from .runner import BatchOptions, run_batch
from .workbench import serve_workbench


def _json_print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _params(args: argparse.Namespace) -> LayoutParams:
    raw: dict[str, Any] = {}
    for attribute, key in (
        ("min_display", "min_display_seconds"),
        ("min_gap", "min_gap_seconds"),
        ("read_speed", "default_max_read_speed"),
        ("description_min", "description_min_seconds"),
        ("description_speed", "description_read_speed"),
    ):
        value = getattr(args, attribute, None)
        if value is not None:
            raw[key] = value
    return LayoutParams.from_dict(raw)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="lcft",
        description="Deterministic local video localization layout toolkit",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("out", type=Path)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument("--sentences", type=int, default=48)
    generate.add_argument("--languages", default="zh,en")
    generate.add_argument("--channels", type=int, choices=(1, 6), default=1)
    generate.add_argument("--frame-rate", default="30")
    generate.add_argument("--description-density", type=int, default=160)
    generate.add_argument("--contention", type=int, default=350)
    generate.add_argument("--read-speed-min", type=int, default=9)
    generate.add_argument("--read-speed-max", type=int, default=17)
    generate.add_argument("--duration", default=None)

    validate = subparsers.add_parser("validate", help="validate and normalize a bundle")
    validate.add_argument("bundle", type=Path)
    validate.add_argument("--out", type=Path)

    run = subparsers.add_parser("run", help="lay out a bundle and publish all tracks")
    run.add_argument("bundle", type=Path)
    run.add_argument("out", type=Path)
    run.add_argument("--workers", type=int, default=2)
    run.add_argument("--global-quota", type=int, default=256 * 1024 * 1024)
    run.add_argument("--job-quota", type=int, default=128 * 1024 * 1024)
    run.add_argument("--description-track", default=None)
    run.add_argument("--min-display", default=None)
    run.add_argument("--min-gap", default=None)
    run.add_argument("--read-speed", default=None)
    run.add_argument("--description-min", default=None)
    run.add_argument("--description-speed", default=None)
    run.add_argument("--no-snapshots", action="store_true")

    recover = subparsers.add_parser(
        "recover", help="resume from the latest validated snapshot/checkpoint"
    )
    recover.add_argument("bundle", type=Path)
    recover.add_argument("snapshot_dir", type=Path)
    recover.add_argument("language")
    recover.add_argument("out", type=Path)
    recover.add_argument("--workers", type=int, default=2)
    recover.add_argument("--fallback", action="store_true")

    workbench = subparsers.add_parser("workbench", help="serve the local interactive UI")
    workbench.add_argument("bundle", type=Path)
    workbench.add_argument("out", type=Path)
    workbench.add_argument("--host", default="127.0.0.1")
    workbench.add_argument("--port", type=int, default=8765)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            from fractions import Fraction

            manifest = generate_sample(
                args.out,
                seed=args.seed,
                sentence_count=args.sentences,
                languages=[item for item in args.languages.split(",") if item],
                channels=args.channels,
                frame_rate=Fraction(args.frame_rate),
                description_density_permille=args.description_density,
                contention_permille=args.contention,
                read_speed_min=args.read_speed_min,
                read_speed_max=args.read_speed_max,
                duration_seconds=args.duration,
            )
            _json_print(manifest)
            return 0
        if args.command == "validate":
            loaded = load_bundle(args.bundle)
            result = loaded.normalized.to_dict()
            if args.out:
                atomic_write_json(args.out, result)
            else:
                _json_print(result)
            return 0
        if args.command == "run":
            options = BatchOptions(
                workers=args.workers,
                global_quota=args.global_quota,
                job_quota=args.job_quota,
                description_track=args.description_track,
                write_snapshots=not args.no_snapshots,
            )
            summary = run_batch(
                args.bundle,
                args.out,
                _params(args),
                options,
            )
            _json_print(summary)
            return 0 if summary.get("ok") else 2
        if args.command == "recover":
            loaded = load_bundle(args.bundle)
            summary = resume_from_snapshot(
                loaded,
                args.snapshot_dir,
                args.language,
                args.out,
                workers=args.workers,
                strict=not args.fallback,
            )
            _json_print(summary)
            return 0
        if args.command == "workbench":
            serve_workbench(args.bundle, args.out, host=args.host, port=args.port)
            return 0
    except LCFTError as exc:
        _json_print(exc.to_dict())
        return 2
    return 1


if __name__ == "__main__":
    raise SystemExit(main())

