"""Command-line interface for the offline validation renderer."""

from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any, Sequence, TextIO

from .errors import BusinessError
from .io_utils import atomic_write_json
from .renderer import render_scene
from .sample_scene import write_sample_scene
from .scene import load_scene
from .verify import compare_outputs


EXIT_OK = 0
EXIT_VALIDATION = 2
EXIT_RENDER = 3
EXIT_MISMATCH = 4


class _InterruptFlag:
    def __init__(self) -> None:
        self.requested = False
        self.signal_number: int | None = None

    def handle(self, signum: int, _frame: Any) -> None:
        self.requested = True
        self.signal_number = signum


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="rosewindow",
        description=(
            "Deterministic offline rose-window validator with chunk snapshots"
        ),
    )
    parser.add_argument(
        "--version", action="version", version="rosewindow 1.0.0"
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample = subparsers.add_parser("sample", help="write the deterministic sample scene")
    sample.add_argument("--output", required=True, type=Path)

    render = subparsers.add_parser("render", help="validate and render a scene JSON")
    render.add_argument("--scene", required=True, type=Path)
    render.add_argument("--out-dir", required=True, type=Path)
    render.add_argument("--snapshot-dir", type=Path)
    render.add_argument(
        "--fresh",
        action="store_true",
        help="discard any committed snapshot and restart from chunk zero",
    )
    render.add_argument(
        "--chunk-delay-ms",
        type=float,
        default=0.0,
        help="verification hook: delay after each committed chunk",
    )
    render.add_argument(
        "--reference",
        type=Path,
        help="optional baseline output directory for a byte-level comparison",
    )
    render.add_argument(
        "--diff-output",
        type=Path,
        help="where to write the structured comparison (default: OUT/resume_diff.json)",
    )
    render.add_argument(
        "--error-report",
        type=Path,
        help="optional path for a structured validation error report",
    )

    verify = subparsers.add_parser(
        "verify", help="compare two completed output directories byte-for-byte"
    )
    verify.add_argument("--reference", required=True, type=Path)
    verify.add_argument("--candidate", required=True, type=Path)
    verify.add_argument("--output", type=Path)
    return parser


def _print_json(value: Any, stream: TextIO | None = None) -> None:
    print(
        json.dumps(value, sort_keys=True, ensure_ascii=True),
        file=sys.stdout if stream is None else stream,
    )


def _render_command(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)

    flag = _InterruptFlag()
    old_handlers: dict[int, Any] = {}
    for signal_name in ("SIGINT", "SIGTERM"):
        signal_value = getattr(signal, signal_name, None)
        if signal_value is None:
            continue
        old_handlers[signal_value] = signal.getsignal(signal_value)
        signal.signal(signal_value, flag.handle)
    try:
        result = render_scene(
            scene,
            args.out_dir,
            snapshot_dir=args.snapshot_dir,
            fresh=args.fresh,
            chunk_delay_ms=max(0.0, float(args.chunk_delay_ms)),
            interrupt_check=lambda: flag.requested,
        )
    finally:
        for signal_value, old_handler in old_handlers.items():
            signal.signal(signal_value, old_handler)

    if result.status == "interrupted":
        _print_json(
            {
                "ok": True,
                "status": "interrupted",
                "exit_code": EXIT_OK,
                "completed_chunks": len(result.state.completed_chunks),
                "total_chunks": scene.chunk_count,
                "snapshot": str(result.snapshot_path),
                "recovery_report": str(args.out_dir / "recovery_report.json"),
                "signal": flag.signal_number,
            }
        )
        return EXIT_OK

    exit_code = EXIT_OK
    comparison: dict[str, Any] | None = None
    if args.reference is not None:
        diff_path = (
            args.diff_output
            if args.diff_output is not None
            else args.out_dir / "resume_diff.json"
        )
        comparison = compare_outputs(args.reference, args.out_dir, output_path=diff_path)
        if not comparison["ok"]:
            exit_code = EXIT_MISMATCH
    _print_json(
        {
            "ok": exit_code == EXIT_OK,
            "status": "complete",
            "scene_digest": scene.digest,
            "output_dir": str(args.out_dir),
            "completed_chunks": len(result.state.completed_chunks),
            "computed_chunks_this_run": len(result.computed_chunks),
            "snapshot_load_status": result.snapshot_load_status,
            "comparison_ok": None if comparison is None else comparison["ok"],
            "exit_code": exit_code,
        }
    )
    return exit_code


def _verify_command(args: argparse.Namespace) -> int:
    result = compare_outputs(args.reference, args.candidate, output_path=args.output)
    _print_json(result)
    return EXIT_OK if result["ok"] else EXIT_MISMATCH


def _normalize_argv(argv: Sequence[str]) -> list[str]:
    arguments = list(argv)
    commands = {"sample", "render", "verify"}
    if not arguments or arguments[0] not in commands:
        # Accept the convenient legacy form:
        #   python -m rosewindow --scene scene.json --out-dir result
        if arguments and arguments[0] in {"-h", "--help", "--version"}:
            return arguments
        return ["render"] + arguments
    return arguments


def main(argv: Sequence[str] | None = None) -> int:
    raw_arguments = list(sys.argv[1:] if argv is None else argv)
    parser = _parser()
    args = parser.parse_args(_normalize_argv(raw_arguments))
    if args.command == "sample":
        args.output.parent.mkdir(parents=True, exist_ok=True)
        write_sample_scene(args.output)
        _print_json(
            {
                "ok": True,
                "status": "sample_written",
                "output": str(args.output),
            }
        )
        return EXIT_OK
    if args.command == "verify":
        return _verify_command(args)
    try:
        return _render_command(args)
    except BusinessError as exc:
        payload = exc.as_dict()
        _print_json(payload, sys.stderr)
        error_report = getattr(args, "error_report", None)
        if error_report is not None:
            try:
                atomic_write_json(error_report, payload)
            except OSError:
                pass
        return EXIT_VALIDATION
    except KeyboardInterrupt:
        _print_json(
            {
                "ok": False,
                "error": {
                    "code": "RW_E_INTERRUPTED",
                    "message": "interrupted before graceful handling was installed",
                },
            },
            sys.stderr,
        )
        return EXIT_RENDER
    except Exception as exc:
        _print_json(
            {
                "ok": False,
                "error": {
                    "code": "RW_E_RENDER",
                    "message": f"{type(exc).__name__}: {exc}",
                },
            },
            sys.stderr,
        )
        return EXIT_RENDER
