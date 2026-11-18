from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any, Sequence

from . import __version__
from .errors import BusinessError, Issue
from .manifest import load_manifest
from .pipeline import RenderConfig, render_project
from .synthesis import create_demo_project


class _InterruptFlag:
    def __init__(self) -> None:
        self.requested = False

    def set(self) -> None:
        self.requested = True

    def __call__(self) -> bool:
        return self.requested


def _print_json(value: Any, *, stream: Any = sys.stdout) -> None:
    stream.write(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    stream.flush()


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="orchestral-master",
        description="离线交响乐分轨对齐、掩蔽解耦、母带渲染与快照恢复",
    )
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    init_parser = subparsers.add_parser("init", help="确定性生成六路分轨、参考通道和清单")
    init_parser.add_argument("--output-dir", required=True, type=Path)
    init_parser.add_argument("--seed", type=int, default=24701)
    init_parser.add_argument("--duration", type=float, default=8.0)
    init_parser.add_argument("--sample-rate", type=int, default=48000)
    init_parser.add_argument("--bit-depth", type=int, choices=(16, 24, 32), default=24)
    init_parser.add_argument("--block-frames", type=int, default=16384)
    init_parser.add_argument("--force", action="store_true")

    validate_parser = subparsers.add_parser("validate", help="校验清单和六路 WAV 格式")
    validate_parser.add_argument("--manifest", required=True, type=Path)

    render_parser = subparsers.add_parser("render", help="渲染立体声母带和质量报告")
    render_parser.add_argument("--manifest", required=True, type=Path)
    render_parser.add_argument("--output", type=Path)
    render_parser.add_argument("--report", type=Path)
    render_parser.add_argument("--snapshot", type=Path)
    render_parser.add_argument("--block-frames", type=int)
    render_parser.add_argument("--fresh", action="store_true", help="明确丢弃旧快照并从零渲染")
    render_parser.add_argument(
        "--block-delay-ms",
        type=float,
        default=0.0,
        help=argparse.SUPPRESS,
    )

    demo_parser = subparsers.add_parser("demo", help="生成演示工程并立即渲染")
    demo_parser.add_argument("--output-dir", required=True, type=Path)
    demo_parser.add_argument("--seed", type=int, default=24701)
    demo_parser.add_argument("--duration", type=float, default=8.0)
    demo_parser.add_argument("--sample-rate", type=int, default=48000)
    demo_parser.add_argument("--bit-depth", type=int, choices=(16, 24, 32), default=24)
    demo_parser.add_argument("--block-frames", type=int, default=16384)
    demo_parser.add_argument("--force", action="store_true")
    return parser


def _paths(manifest_path: Path, args: argparse.Namespace) -> tuple[Path, Path, Path]:
    root = manifest_path.resolve().parent
    output = args.output.resolve() if args.output else root / "master.wav"
    report = args.report.resolve() if args.report else root / "report.json"
    snapshot = args.snapshot.resolve() if args.snapshot else root / "checkpoint.json"
    return output, report, snapshot


def _run_render(args: argparse.Namespace) -> int:
    manifest_path = args.manifest.resolve()
    output, report, snapshot = _paths(manifest_path, args)
    interrupt = _InterruptFlag()
    previous_handler = signal.getsignal(signal.SIGINT)

    def handle_interrupt(_signum: int, _frame: Any) -> None:
        interrupt.set()

    signal.signal(signal.SIGINT, handle_interrupt)
    try:
        result = render_project(
            RenderConfig(
                manifest_path=manifest_path,
                output_path=output,
                report_path=report,
                snapshot_path=snapshot,
                block_frames=args.block_frames,
                fresh=args.fresh,
                block_delay_seconds=max(0.0, args.block_delay_ms / 1000.0),
                stop_requested=interrupt,
            )
        )
    finally:
        signal.signal(signal.SIGINT, previous_handler)
    status = result.get("status")
    summary = {
        "ok": True,
        "status": status,
        "output": str(output) if status == "complete" else None,
        "report": str(report),
        "snapshot": str(snapshot),
        "completed_blocks": result.get("snapshot", {}).get("completed_blocks"),
        "total_blocks": result.get("snapshot", {}).get("total_blocks"),
        "loudness_lufs": result.get("master", {}).get("loudness_lufs_after_limiter"),
        "true_peak_dbtp": result.get("master", {}).get("true_peak_dbtp_after_limiter"),
    }
    _print_json(summary)
    warnings = result.get("warnings", [])
    if warnings:
        _print_json({"ok": True, "warnings": warnings}, stream=sys.stderr)
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "init":
            manifest_path = create_demo_project(
                args.output_dir,
                seed=args.seed,
                duration_seconds=args.duration,
                sample_rate=args.sample_rate,
                bit_depth=args.bit_depth,
                block_frames=args.block_frames,
                force=args.force,
            )
            _print_json(
                {
                    "ok": True,
                    "status": "initialized",
                    "manifest": str(manifest_path),
                    "audio_dir": str(manifest_path.parent / "audio"),
                }
            )
            return 0
        if args.command == "validate":
            manifest = load_manifest(args.manifest)
            _print_json(
                {
                    "ok": True,
                    "status": "valid",
                    "manifest": str(manifest.path),
                    "tracks": len(manifest.tracks),
                    "sample_rate": manifest.sample_rate,
                    "bit_depth": manifest.bit_depth,
                    "channels": manifest.channels,
                    "seed": manifest.seed,
                }
            )
            return 0
        if args.command == "render":
            return _run_render(args)
        if args.command == "demo":
            manifest_path = create_demo_project(
                args.output_dir,
                seed=args.seed,
                duration_seconds=args.duration,
                sample_rate=args.sample_rate,
                bit_depth=args.bit_depth,
                block_frames=args.block_frames,
                force=args.force,
            )
            render_args = parser.parse_args(
                [
                    "render",
                    "--manifest",
                    str(manifest_path),
                    "--block-frames",
                    str(args.block_frames),
                ]
            )
            return _run_render(render_args)
        parser.error("未知命令")
        return 2
    except BusinessError as exc:
        _print_json(exc.payload(), stream=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        _print_json(
            {
                "ok": False,
                "errors": [
                    {
                        "code": "E_INTERRUPTED",
                        "channel": "process",
                        "message": "在可提交块边界之前收到中断",
                    }
                ],
            },
            stream=sys.stderr,
        )
        return 0
    except Exception as exc:
        _print_json(
            {
                "ok": False,
                "errors": [
                    {
                        "code": "E_RENDER_INTERNAL",
                        "channel": "render",
                        "message": str(exc),
                        "details": {"type": type(exc).__name__},
                    }
                ],
            },
            stream=sys.stderr,
        )
        return 4


if __name__ == "__main__":
    raise SystemExit(main())

