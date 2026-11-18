"""Command-line interface for generation, rendering and verification."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .errors import business_error, write_json
from .generator import generate_channels, write_demo
from .manifest import load_manifest, validate_manifest_document
from .render import render


def _print_report(report: dict[str, Any]) -> None:
    print(
        json.dumps(
            report,
            ensure_ascii=False,
            indent=2,
            allow_nan=False,
        )
    )


def _report_paths(output: Path, report: str | None) -> tuple[Path, Path]:
    report_path = (
        Path(report)
        if report
        else output.with_name(output.stem + ".report.json")
    )
    recovery_path = report_path.with_name(report_path.stem + ".recovery.json")
    return report_path, recovery_path


def _cmd_demo(args: argparse.Namespace) -> int:
    manifest_path, master_path = write_demo(args.directory)
    _print_report(
        {
            "status": "ok",
            "manifest": str(manifest_path),
            "suggested_master": str(master_path),
        }
    )
    return 0


def _cmd_generate(args: argparse.Namespace) -> int:
    document, errors = load_manifest(args.manifest)
    if errors:
        _print_report(
            {
                "status": "error",
                "hard_errors": errors,
                "master_created": False,
            }
        )
        return 2
    assert document is not None
    errors = validate_manifest_document(document)
    if errors:
        _print_report(
            {
                "status": "error",
                "hard_errors": errors,
                "master_created": False,
            }
        )
        return 2
    try:
        channels = generate_channels(document, args.manifest)
    except (OSError, ValueError) as exc:
        _print_report(
            {
                "status": "error",
                "hard_errors": [
                    business_error(
                        "GENERATION_FAILED",
                        None,
                        "deterministic source generation failed",
                        reason=str(exc).splitlines()[0][:240],
                    )
                ],
                "master_created": False,
            }
        )
        return 2
    _print_report(
        {
            "status": "ok",
            "manifest": Path(args.manifest).name,
            "channels": channels,
        }
    )
    return 0


def _cmd_render(args: argparse.Namespace) -> int:
    output = Path(args.out)
    report_path, recovery_path = _report_paths(output, args.report)
    if args.recovery_report:
        recovery_path = Path(args.recovery_report)
    state_dir = (
        Path(args.state_dir)
        if args.state_dir
        else output.parent / ".wuchang_state"
    )
    try:
        code, report = render(
            manifest_path=args.manifest,
            output_path=output,
            report_path=report_path,
            state_dir=state_dir,
            recovery_report_path=recovery_path,
            fresh=args.fresh,
        )
    except Exception as exc:  # noqa: BLE001 - structured CLI boundary
        import os
        import traceback

        if os.environ.get("WUCHANG_DEBUG"):
            traceback.print_exc()
        report = {
            "status": "error",
            "schema_version": 1,
            "hard_errors": [
                business_error(
                    "RENDER_FAILED",
                    None,
                    "unexpected renderer failure",
                    reason=f"{type(exc).__name__}: {str(exc).splitlines()[0][:300]}",
                )
            ],
            "warnings": [],
            "master_created": False,
        }
        write_json(report_path, report)
        code = 4
    if not args.quiet:
        _print_report(report)
    return code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="wuchang",
        description="Deterministic Peking-opera percussion bus renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    demo = subparsers.add_parser(
        "demo", help="write the deterministic five-channel demo session"
    )
    demo.add_argument(
        "--directory",
        default="demo",
        help="destination directory (default: demo)",
    )
    demo.set_defaults(func=_cmd_demo)

    generate = subparsers.add_parser(
        "generate", help="synthesize WAVs declared by a manifest"
    )
    generate.add_argument("--manifest", required=True)
    generate.set_defaults(func=_cmd_generate)

    render_parser = subparsers.add_parser(
        "render", help="align, compensate, master and write a stereo WAV"
    )
    render_parser.add_argument("--manifest", required=True)
    render_parser.add_argument("--out", required=True)
    render_parser.add_argument("--report", default=None)
    render_parser.add_argument("--recovery-report", default=None)
    render_parser.add_argument("--state-dir", default=None)
    render_parser.add_argument(
        "--fresh",
        action="store_true",
        help="discard any existing state before rendering",
    )
    render_parser.add_argument("--quiet", action="store_true")
    render_parser.set_defaults(func=_cmd_render)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    sys.exit(main())

