"""Command-line interface."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .errors import BusinessError
from .fixture import generate_demo
from .pipeline import align_project


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="drum_align",
        description="Offline deterministic multi-camera drum stem alignment",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    render = subparsers.add_parser("render", help="align tracks and render the master")
    render.add_argument("--project", required=True, type=Path)
    render.add_argument("--master", type=Path)
    render.add_argument("--report", type=Path)
    render.add_argument("--snapshot", type=Path)
    render.add_argument(
        "--oneshot",
        action="store_true",
        help="disable checkpoint reuse (same block math, no resume state)",
    )
    render.add_argument(
        "--stop-after-blocks",
        type=int,
        default=None,
        help=argparse.SUPPRESS,
    )

    demo = subparsers.add_parser(
        "generate-demo", help="write a deterministic synthetic camera project"
    )
    demo.add_argument("--output-dir", required=True, type=Path)
    demo.add_argument("--duration-seconds", type=float, default=26.0)
    return parser


def _print_error(error: BusinessError) -> None:
    payload = {
        "ok": False,
        "status": "error",
        "errors": [error.to_dict()],
        "warnings": [],
        "quality_flags": [],
    }
    sys.stderr.write(json.dumps(payload, ensure_ascii=False, indent=2) + "\n")


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "generate-demo":
        try:
            project_path = generate_demo(args.output_dir, args.duration_seconds)
        except (OSError, ValueError) as exc:
            sys.stderr.write(
                json.dumps(
                    {
                        "ok": False,
                        "status": "error",
                        "errors": [
                            {
                                "code": "E_OUTPUT",
                                "severity": "hard",
                                "camera": None,
                                "reason": str(exc),
                            }
                        ],
                    },
                    ensure_ascii=False,
                    indent=2,
                )
                + "\n"
            )
            return 2
        print(project_path)
        return 0

    if args.stop_after_blocks is not None and args.stop_after_blocks < 1:
        _print_error(
            BusinessError(
                "E_PROJECT_INVALID", "--stop-after-blocks must be positive"
            )
        )
        return 2
    try:
        result = align_project(
            args.project,
            master_path=args.master,
            report_path=args.report,
            snapshot_path=args.snapshot,
            oneshot=args.oneshot,
            interrupt_after_blocks=args.stop_after_blocks,
            warning_sink=lambda event: sys.stderr.write(
                json.dumps(event, ensure_ascii=False) + "\n"
            ),
        )
    except BusinessError as exc:
        _print_error(exc)
        return 2
    except KeyboardInterrupt:
        _print_error(
            BusinessError(
                "E_INTERRUPTED_TOO_EARLY",
                "interrupt arrived before a resumable render block was committed",
            )
        )
        return 0
    except OSError as exc:
        _print_error(
            BusinessError("E_OUTPUT", f"filesystem failure: {type(exc).__name__}")
        )
        return 2
    except Exception as exc:
        _print_error(
            BusinessError(
                "E_INTERNAL",
                f"unexpected failure: {type(exc).__name__}",
            )
        )
        return 2

    summary = {
        "ok": True,
        "status": result.status,
        "report": result.report_path.name,
        "master": result.master_path.name if result.status == "complete" else None,
        "completed_blocks": result.report["render"]["completed_blocks"],
        "total_blocks": result.report["render"]["total_blocks"],
        "resume_count": result.report["resume_count"],
        "recovery_events": result.report["recovery_events"],
    }
    print(json.dumps(summary, ensure_ascii=False, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
