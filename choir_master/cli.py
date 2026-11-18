"""Command-line interface for deterministic generation and mastering."""

from __future__ import annotations

import argparse
import json
import signal
import sys
import traceback
from pathlib import Path
from typing import Any

from . import __version__
from .errors import (
    BusinessError,
    EXIT_OK,
    EXIT_PROCESSING,
    issue,
)
from .generate import generate_project_files, init_workspace
from .manifest import input_digest, load_project, manifest_digest
from .render import prepare_render, render_project


def _print_json(value: dict[str, Any], *, stream: Any = sys.stdout) -> None:
    stream.write(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


def _install_graceful_handlers(handler: Any) -> dict[int, Any]:
    previous: dict[int, Any] = {}
    for name in ("SIGINT", "SIGTERM", "SIGBREAK"):
        signum = getattr(signal, name, None)
        if signum is None:
            continue
        try:
            previous[signum] = signal.signal(signum, handler)
        except (OSError, ValueError):
            continue
    return previous


def _restore_handlers(previous: dict[int, Any]) -> None:
    for signum, handler in previous.items():
        try:
            signal.signal(signum, handler)
        except (OSError, ValueError):
            pass


def _add_init_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--workspace",
        default="demo",
        help="workspace directory for project.json and audio/ (default: demo)",
    )
    parser.add_argument("--seed", type=int, default=235)
    parser.add_argument("--duration", type=float, default=8.0)
    parser.add_argument("--block-frames", type=int, default=24000)
    parser.add_argument("--loudness", type=float, default=-20.0)
    parser.add_argument("--true-peak", type=float, default=-1.0)
    parser.add_argument("--bit-depth", type=int, choices=(16, 24), default=24)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="choir-master",
        description="Deterministic offline choir mastering renderer",
    )
    parser.add_argument("--version", action="version", version=__version__)
    parser.add_argument(
        "--debug",
        action="store_true",
        help="show tracebacks for unexpected internal errors",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    init_parser = subparsers.add_parser(
        "init",
        help="create a deterministic manifest and eight WAV files",
    )
    _add_init_arguments(init_parser)

    generate_parser = subparsers.add_parser(
        "generate",
        help="regenerate all WAV files declared by an existing manifest",
    )
    generate_parser.add_argument("--manifest", required=True)

    validate_parser = subparsers.add_parser(
        "validate",
        help="validate the manifest and all declared WAV files",
    )
    validate_parser.add_argument("--manifest", required=True)

    prepare_parser = subparsers.add_parser(
        "prepare",
        help="run analysis and print stable summary checks without rendering",
    )
    prepare_parser.add_argument("--manifest", required=True)

    render_parser = subparsers.add_parser(
        "render",
        help="render a stereo master with a resumable block snapshot",
    )
    render_parser.add_argument("--manifest", required=True)
    render_parser.add_argument("--output", default="master.wav")
    render_parser.add_argument("--report", default="report.json")
    render_parser.add_argument("--snapshot", default="master.snapshot")
    render_parser.add_argument(
        "--block-frames",
        type=int,
        default=None,
        help="override manifest block_frames for this render",
    )
    render_parser.add_argument(
        "--fresh",
        action="store_true",
        help="discard an existing snapshot before rendering",
    )
    render_parser.add_argument(
        "--block-pause-ms",
        type=float,
        default=0.0,
        help="fault-injection pause after each committed block",
    )

    demo_parser = subparsers.add_parser(
        "demo",
        help="init a workspace and immediately render it",
    )
    _add_init_arguments(demo_parser)
    demo_parser.add_argument("--output", default=None)
    demo_parser.add_argument("--report", default=None)
    demo_parser.add_argument("--snapshot", default=None)
    demo_parser.add_argument("--block-pause-ms", type=float, default=0.0)
    return parser


def _command_init(args: argparse.Namespace) -> int:
    manifest_path = init_workspace(
        args.workspace,
        seed=args.seed,
        duration_seconds=args.duration,
        block_frames=args.block_frames,
        target_loudness_lufs=args.loudness,
        true_peak_limit_dbtp=args.true_peak,
        bit_depth=args.bit_depth,
    )
    project = load_project(manifest_path, check_files=True)
    _print_json(
        {
            "status": "ok",
            "command": "init",
            "manifest": manifest_path.name,
            "manifest_sha256": manifest_digest(project.manifest),
            "input_sha256": input_digest(project),
            "seed": project.seed,
            "microphone_count": len(project.microphones),
            "sample_rate": project.sample_rate,
            "bit_depth": project.bit_depth,
        }
    )
    return EXIT_OK


def _command_generate(args: argparse.Namespace) -> int:
    summary = generate_project_files(args.manifest, check_files=False)
    summary["status"] = "ok"
    summary["command"] = "generate"
    _print_json(summary)
    return EXIT_OK


def _command_validate(args: argparse.Namespace) -> int:
    project = load_project(args.manifest, check_files=True)
    _print_json(
        {
            "status": "ok",
            "command": "validate",
            "manifest": project.manifest_path.name,
            "manifest_sha256": manifest_digest(project.manifest),
            "input_sha256": input_digest(project),
            "voice_count": len(project.voices),
            "microphone_count": len(project.microphones),
            "sample_rate": project.sample_rate,
            "bit_depth": project.bit_depth,
            "target_loudness_lufs": project.target_loudness_lufs,
            "true_peak_limit_dbtp": project.true_peak_limit_dbtp,
        }
    )
    return EXIT_OK


def _command_prepare(args: argparse.Namespace) -> int:
    project = load_project(args.manifest, check_files=True)
    prepared = prepare_render(project)
    _print_json(
        {
            "status": "ok",
            "command": "prepare",
            "manifest_sha256": prepared.manifest_sha256,
            "input_sha256": prepared.input_sha256,
            "total_frames": prepared.total_frames,
            "block_frames": prepared.block_frames,
            "breakpoint_intervals": prepared.analysis["voice_intervals"],
            "voice_correction_gains": prepared.analysis[
                "voice_correction_gains"
            ],
            "compensation": [item.as_dict() for item in prepared.compensation],
            "master_loudness_lufs": round(
                prepared.master.integrated_loudness_lufs, 6
            ),
            "true_peak_dbtp": round(prepared.master.true_peak_dbtp, 6),
            "limiter_triggered_samples": (
                prepared.master.limiter_triggered_samples
            ),
        }
    )
    return EXIT_OK


def _command_render(args: argparse.Namespace) -> int:
    interrupt_requested = {"value": False}

    def handle_sigint(_signum: int, _frame: Any) -> None:
        interrupt_requested["value"] = True

    previous = _install_graceful_handlers(handle_sigint)
    try:
        outcome = render_project(
            args.manifest,
            output_path=args.output,
            report_path=args.report,
            snapshot_dir=args.snapshot,
            block_frames=args.block_frames,
            fresh=args.fresh,
            block_pause_ms=args.block_pause_ms,
            should_interrupt=lambda: interrupt_requested["value"],
        )
    finally:
        _restore_handlers(previous)
    _print_json({"command": "render", **outcome.as_dict()})
    return EXIT_OK


def _command_demo(args: argparse.Namespace) -> int:
    manifest_path = init_workspace(
        args.workspace,
        seed=args.seed,
        duration_seconds=args.duration,
        block_frames=args.block_frames,
        target_loudness_lufs=args.loudness,
        true_peak_limit_dbtp=args.true_peak,
        bit_depth=args.bit_depth,
    )
    root = Path(args.workspace)
    output = args.output or str(root / "master.wav")
    report = args.report or str(root / "report.json")
    snapshot = args.snapshot or str(root / "master.snapshot")
    interrupt_requested = {"value": False}

    def handle_sigint(_signum: int, _frame: Any) -> None:
        interrupt_requested["value"] = True

    previous = _install_graceful_handlers(handle_sigint)
    try:
        outcome = render_project(
            manifest_path,
            output_path=output,
            report_path=report,
            snapshot_dir=snapshot,
            fresh=True,
            block_pause_ms=args.block_pause_ms,
            should_interrupt=lambda: interrupt_requested["value"],
        )
    finally:
        _restore_handlers(previous)
    result = {"command": "demo", **outcome.as_dict()}
    result["manifest"] = manifest_path.name
    _print_json(result)
    return EXIT_OK


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "init":
            return _command_init(args)
        if args.command == "generate":
            return _command_generate(args)
        if args.command == "validate":
            return _command_validate(args)
        if args.command == "prepare":
            return _command_prepare(args)
        if args.command == "render":
            return _command_render(args)
        if args.command == "demo":
            return _command_demo(args)
        parser.error("unknown command")
        return 2
    except BusinessError as exc:
        _print_json(exc.payload(), stream=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        _print_json(
            {
                "status": "error",
                "errors": [
                    issue(
                        "E_INTERRUPTED",
                        "<master>",
                        "operation was interrupted outside a render block",
                    ).as_dict()
                ],
            },
            stream=sys.stderr,
        )
        return 130
    except Exception as exc:
        if getattr(args, "debug", False):
            traceback.print_exc()
        _print_json(
            {
                "status": "error",
                "errors": [
                    issue(
                        "E_INTERNAL",
                        "<master>",
                        f"unexpected internal error: {type(exc).__name__}",
                    ).as_dict()
                ],
            },
            stream=sys.stderr,
        )
        return EXIT_PROCESSING

