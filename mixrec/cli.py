"""Command line entry point for generation, rendering and verification."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .engine import MixEngine, summarize_index
from .errors import MixError
from .generator import generate_samples


def _print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _load_engine(args: argparse.Namespace) -> MixEngine:
    return MixEngine.from_file(
        args.config,
        output_dir_override=getattr(args, "output_dir", None),
        trace_override=getattr(args, "trace", None),
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m mixrec", description="Deterministic local mix recording workbench")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", aliases=["samples"], help="generate deterministic local fixtures")
    generate.add_argument("--out", required=True, help="fixture root directory")
    generate.add_argument("--seed", type=int, default=20261121)
    generate.add_argument("--no-baseline", action="store_true", help="skip baseline rendering")

    render = sub.add_parser("render", help="render or continue a job")
    render.add_argument("config")
    render.add_argument("--trace")
    render.add_argument("--output-dir")
    render.add_argument("--no-resume", action="store_true")
    render.add_argument("--max-slices", type=int)

    resume = sub.add_parser("resume", help="validate and resume a committed prefix")
    resume.add_argument("config")
    resume.add_argument("--trace")
    resume.add_argument("--output-dir")
    resume.add_argument("--max-slices", type=int)

    update = sub.add_parser("update", help="incrementally update one track")
    update.add_argument("config")
    update.add_argument("--track", required=True)
    update.add_argument("--gain-db", type=float)
    update.add_argument("--delay-samples", type=int)
    update.add_argument(
        "--mute",
        action="append",
        default=None,
        metavar="START:END",
        help="mute range in seconds; repeat for multiple ranges",
    )
    update.add_argument("--trace")
    update.add_argument("--output-dir")

    verify = sub.add_parser("verify", help="validate index chain and every slice hash")
    verify.add_argument("config")
    verify.add_argument("--output-dir")

    gui = sub.add_parser("gui", help="open the PySide6 workbench")
    gui.add_argument("--config")

    demo = sub.add_parser("demo", help="generate fixtures and render the short sample")
    demo.add_argument("--out", required=True)
    demo.add_argument("--seed", type=int, default=20261121)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command in {"generate", "samples"}:
            manifest = generate_samples(args.out, seed=args.seed, with_baseline=not args.no_baseline)
            _print(manifest)
            return 0
        if args.command == "render":
            engine = _load_engine(args)
            result = engine.run(
                resume=not args.no_resume,
                fresh=args.no_resume,
                max_slices=args.max_slices,
            )
            _print(result.summary)
            return 0
        if args.command == "resume":
            engine = _load_engine(args)
            result = engine.run(resume=True, max_slices=args.max_slices)
            _print(result.summary)
            return 0
        if args.command == "update":
            ranges = None
            if args.mute is not None:
                ranges = []
                for value in args.mute:
                    try:
                        start, end = value.split(":", 1)
                        ranges.append([float(start), float(end)])
                    except ValueError as exc:
                        raise MixError("E_CONFIG", f"invalid mute range: {value}") from exc
            engine = _load_engine(args)
            index = engine.update_track(
                args.track,
                gain_db=args.gain_db,
                delay_samples=args.delay_samples,
                mute_ranges=ranges,
            )
            _print(summarize_index(index))
            return 0
        if args.command == "verify":
            engine = _load_engine(args)
            _print(summarize_index(engine.verify()))
            return 0
        if args.command == "gui":
            from .gui import launch

            return launch(args.config)
        if args.command == "demo":
            manifest = generate_samples(args.out, seed=args.seed, with_baseline=False)
            config = Path(args.out) / "jobs" / "short_timeline" / "config.json"
            engine = MixEngine.from_file(config)
            result = engine.run(resume=False)
            _print({"manifest": manifest, "result": result.summary})
            return 0
    except MixError as exc:
        _print(exc.to_dict())
        return 2
    return 1


if __name__ == "__main__":
    sys.exit(main())
