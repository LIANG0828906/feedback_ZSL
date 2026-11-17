"""Command-line interface."""

from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any

from . import __version__
from .control import StopFlag
from .errors import MasterError, MemoryLimit, ProjectInvalid, RenderFailed


class _ArgumentParser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        raise ProjectInvalid(message, field="arguments")


def _build_parser() -> argparse.ArgumentParser:
    parser = _ArgumentParser(prog="biomemaster")
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    render = subparsers.add_parser(
        "render", help="render a project to a normalized WAV master"
    )
    render.add_argument("project", help="project JSON file")
    render.add_argument("--output", required=True, help="destination WAV path")
    render.add_argument("--report", required=True, help="destination QC JSON path")
    render.add_argument(
        "--work-dir",
        default=None,
        help="temporary checkpoint directory (default: <output>.work)",
    )
    render.add_argument(
        "--workers",
        type=int,
        default=1,
        help="parallel biome workers (layers within a biome remain serial)",
    )

    verify = subparsers.add_parser(
        "verify", help="verify a QC report against its WAV master"
    )
    verify.add_argument("--report", required=True, help="QC report JSON")
    verify.add_argument("--wav", required=True, help="WAV master")
    return parser


def _install_signal_handlers(stop: StopFlag) -> list[tuple[int, Any]]:
    previous: list[tuple[int, Any]] = []
    candidates = [signal.SIGINT, signal.SIGTERM]
    if hasattr(signal, "SIGBREAK"):
        candidates.append(signal.SIGBREAK)
    for signum in candidates:
        old = signal.getsignal(signum)
        signal.signal(signum, stop.request)
        previous.append((signum, old))
    return previous


def _restore_signal_handlers(previous: list[tuple[int, Any]]) -> None:
    for signum, handler in previous:
        signal.signal(signum, handler)


def _error_payload(error: MasterError) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "ok": False,
        "code": error.code,
        "message": error.message,
    }
    if error.details:
        payload["details"] = error.details
    return payload


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")))


def _render(args: argparse.Namespace, stop: StopFlag) -> int:
    from .project import load_project
    from .render import run_render

    if args.workers < 1:
        raise ProjectInvalid("--workers must be >= 1", field="--workers")
    output_path = Path(args.output).expanduser().resolve()
    report_path = Path(args.report).expanduser().resolve()
    if output_path == report_path:
        raise ProjectInvalid("--output and --report must be different paths")
    work_dir = (
        Path(args.work_dir).expanduser().resolve()
        if args.work_dir
        else output_path.with_name(output_path.name + ".work")
    )
    for destination_name, destination in (
        ("--output", output_path),
        ("--report", report_path),
    ):
        if destination == work_dir or work_dir in destination.parents:
            raise ProjectInvalid(
                f"{destination_name} must not be inside --work-dir",
                field=destination_name,
                work_dir=str(work_dir),
            )
    project = load_project(args.project, workers=args.workers)
    result = run_render(
        project,
        output_path=output_path,
        report_path=report_path,
        work_dir=work_dir,
        workers=args.workers,
        stop=stop,
    )
    if result.get("status") == "complete":
        report = result["report"]
        _print_json(
            {
                "ok": True,
                "status": "complete",
                "output": str(output_path),
                "report": str(report_path),
                "wav_sha256": report["wav_sha256"],
                "integrated_loudness_lufs": report["integrated_loudness_lufs"],
                "true_peak_dbtp": report["true_peak_dbtp"],
            }
        )
    else:
        _print_json(
            {
                "ok": True,
                "status": "interrupted",
                "message": "current block checkpointed; rerun to resume",
                **result,
            }
        )
    return 0


def _verify(args: argparse.Namespace) -> int:
    from .report import verify_report

    result = verify_report(
        Path(args.report).expanduser().resolve(),
        Path(args.wav).expanduser().resolve(),
    )
    _print_json(result)
    return 0


def main(argv: list[str] | None = None) -> int:
    stop = StopFlag()
    previous_handlers: list[tuple[int, Any]] = []
    try:
        parser = _build_parser()
        args = parser.parse_args(argv)
        previous_handlers = _install_signal_handlers(stop)
        if args.command == "render":
            return _render(args, stop)
        if args.command == "verify":
            return _verify(args)
        raise ProjectInvalid(f"unknown command: {args.command}")
    except MasterError as error:
        print(
            json.dumps(
                _error_payload(error),
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ),
            file=sys.stderr,
        )
        return error.exit_code
    except MemoryError:
        wrapped = MemoryLimit("process memory was exhausted")
        print(
            json.dumps(
                _error_payload(wrapped),
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ),
            file=sys.stderr,
        )
        return wrapped.exit_code
    except Exception as error:  # noqa: BLE001 - final CLI error boundary
        wrapped = RenderFailed(
            "unexpected renderer failure",
            error_type=type(error).__name__,
            error=str(error),
        )
        print(
            json.dumps(
                _error_payload(wrapped),
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ),
            file=sys.stderr,
        )
        return wrapped.exit_code
    finally:
        if previous_handlers:
            _restore_signal_handlers(previous_handlers)


if __name__ == "__main__":
    raise SystemExit(main())
