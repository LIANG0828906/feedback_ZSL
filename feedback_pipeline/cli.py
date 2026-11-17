"""Command line interface.

Usage:
    python -m feedback_pipeline generate --out demo [--seed 7] [--segment 2]
    python -m feedback_pipeline run --project demo/project.json --out demo/out
"""

from __future__ import annotations

import argparse
import json
import signal
import sys
import threading

from . import errors
from .pipeline import InterruptFlag, run_pipeline
from .synth import generate_project


def _cmd_generate(args: argparse.Namespace) -> int:
    path = generate_project(args.out, seed=args.seed, song_segment=args.segment,
                            duration=args.duration)
    print(json.dumps({"project": str(path)}, sort_keys=True))
    return 0


def _cmd_run(args: argparse.Namespace) -> int:
    flag = InterruptFlag()
    previous = None
    installed = threading.current_thread() is threading.main_thread()
    if installed:
        previous = signal.getsignal(signal.SIGINT)

        def _handler(signum, frame):
            flag.requested = True

        signal.signal(signal.SIGINT, _handler)
    try:
        report = run_pipeline(
            args.project,
            args.out,
            block_size=args.block_size,
            block_delay_ms=args.block_delay_ms,
            interrupt_flag=flag,
        )
    except errors.PipelineError as exc:
        errors.emit_error(exc)
        return 3
    except KeyboardInterrupt:
        return 0
    finally:
        if installed:
            signal.signal(signal.SIGINT, previous)
    for event in report.get("resume_events", []):
        sys.stderr.write(json.dumps({"resume_event": event}, ensure_ascii=False,
                                    sort_keys=True) + "\n")
    print(json.dumps({"status": report["status"]}, sort_keys=True))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="feedback_pipeline")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="synthesise deterministic multitrack material")
    gen.add_argument("--out", required=True, help="output project directory")
    gen.add_argument("--seed", type=int, default=7)
    gen.add_argument("--segment", type=int, default=2, help="song-segment id")
    gen.add_argument("--duration", type=float, default=6.0)
    gen.set_defaults(func=_cmd_generate)

    run = sub.add_parser("run", help="trace, remove and render the stereo bus")
    run.add_argument("--project", required=True, help="project JSON path")
    run.add_argument("--out", required=True, help="output directory")
    run.add_argument("--block-size", type=int, default=None,
                     help="render block size in samples (default: project value)")
    run.add_argument("--block-delay-ms", type=int, default=0,
                     help="artificial per-block delay (testing hook)")
    run.set_defaults(func=_cmd_run)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)
