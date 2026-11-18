"""Command-line interface for deterministic offline mastering."""

from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
from pathlib import Path
from typing import Any

from .config import load_and_validate
from .dsp import integrated_loudness, true_peak_dbtp
from .errors import BusinessError, emit_error
from .render import inspect_packages, render_book
from .synth import generate_packages
from .wavio import read_wav


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m audiobook_master",
        description="Deterministic, resumable offline audiobook mastering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="deterministically synthesize manifest packages"
    )
    generate.add_argument("--rules", required=True)
    generate.add_argument("--manifest", required=True)

    inspect = subparsers.add_parser(
        "inspect", help="validate and inspect all package statistics"
    )
    inspect.add_argument("--rules", required=True)
    inspect.add_argument("--manifest", required=True)

    validate = subparsers.add_parser(
        "validate", help="validate rules and manifest without rendering"
    )
    validate.add_argument("--rules", required=True)
    validate.add_argument("--manifest", required=True)

    render = subparsers.add_parser(
        "render", help="render the full audiobook master and chapter report"
    )
    render.add_argument("--rules", required=True)
    render.add_argument("--manifest", required=True)
    render.add_argument("--out-master", "--master", dest="master", required=True)
    render.add_argument("--out-report", "--report", dest="report", required=True)
    render.add_argument("--state-dir")
    render.add_argument(
        "--interrupt-after-blocks",
        type=int,
        default=None,
        help="test hook: cooperatively stop after N committed blocks",
    )

    verify = subparsers.add_parser(
        "verify", help="recompute report invariants from the final master"
    )
    verify.add_argument("--master", required=True)
    verify.add_argument("--report", required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "generate":
            payload = generate_packages(args.rules, args.manifest)
        elif args.command == "inspect":
            payload = inspect_packages(args.rules, args.manifest)
        elif args.command == "validate":
            payload = _validate(args.rules, args.manifest)
        elif args.command == "render":
            payload = _render(args)
        elif args.command == "verify":
            payload = _verify(args.master, args.report)
        else:
            raise AssertionError(args.command)
    except BusinessError as error:
        emit_error(error)
        return 2
    except KeyboardInterrupt:
        # A user interrupt is transformed into a cooperative stop by _render.
        # Reaching this branch means it occurred before rendering began.
        payload = {
            "ok": False,
            "error": {
                "code": "E_INTERRUPTED",
                "message": "interrupted before the first rendered block",
                "path": None,
            },
        }
        json.dump(payload, sys.stdout, ensure_ascii=False)
        sys.stdout.write("\n")
        return 0
    json.dump(payload, sys.stdout, ensure_ascii=False, sort_keys=False)
    sys.stdout.write("\n")
    sys.stdout.flush()
    return 0


def _render(args: argparse.Namespace) -> dict[str, Any]:
    master = Path(args.master)
    report = Path(args.report)
    state_dir = (
        Path(args.state_dir)
        if args.state_dir
        else master.parent / f".{master.stem}.state"
    )
    stop_event = threading.Event()
    previous_handlers: dict[int, Any] = {}

    def request_stop(signum: int, frame: Any) -> None:
        del signum, frame
        stop_event.set()

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous_handlers[signum] = signal.getsignal(signum)
        signal.signal(signum, request_stop)
    try:
        return render_book(
            args.rules,
            args.manifest,
            master,
            report,
            state_dir,
            stop_requested=stop_event.is_set,
            interrupt_after_blocks=args.interrupt_after_blocks,
        )
    finally:
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)


def _validate(rules_path: str, manifest_path: str) -> dict[str, Any]:
    book = load_and_validate(rules_path, manifest_path)
    return {
        "ok": True,
        "status": "valid",
        "chapters": len(book.chapters),
        "packages": sum(len(chapter.packages) for chapter in book.chapters),
        "frames": sum(
            chapter.plan.rule.expected_frames for chapter in book.chapters
        ),
        "channels": book.master_channels,
        "sample_rate": book.rules.sample_rate,
        "bit_depth": book.rules.bit_depth,
        "rule_hash": book.rules.content_hash,
        "input_digest": book.input_digest,
    }


def _verify(master_path: str, report_path: str) -> dict[str, Any]:
    master = read_wav(Path(master_path), field_path="master")
    try:
        report = json.loads(Path(report_path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise BusinessError(
            "E_REPORT_INVALID", f"cannot read report: {exc}", "report"
        ) from exc
    sample_rate = master.info.sample_rate
    try:
        relative_gate = float(report["rules"]["relative_gate_lu"])
        expected_frames = int(report["master"]["frames"])
        target = float(report["rules"]["target_loudness_lufs"])
        peak_limit = float(report["rules"]["true_peak_dbtp"])
    except (KeyError, TypeError, ValueError) as exc:
        raise BusinessError(
            "E_REPORT_INVALID", "report is missing required fields", "report"
        ) from exc
    loudness = integrated_loudness(
        master.samples,
        sample_rate,
        relative_gate_lu=relative_gate,
    )
    peak = true_peak_dbtp(master.samples)
    checks = {
        "frames_match": master.info.frames == expected_frames,
        "loudness_matches_report": abs(
            loudness - float(report["loudness"]["after_lufs"])
        )
        <= 1e-6,
        "loudness_reaches_target": abs(loudness - target) <= 0.10,
        "true_peak_within_limit": peak <= peak_limit + 1e-6,
    }
    return {
        "ok": all(checks.values()),
        "status": "verified" if all(checks.values()) else "failed",
        "master_frames": master.info.frames,
        "master_loudness_lufs": round(loudness, 9),
        "master_true_peak_dbtp": round(peak, 9),
        "checks": checks,
    }
