from __future__ import annotations

import argparse
import json
import signal
import sys
from typing import Any, Sequence

from .errors import EXIT_RENDER, BusinessError
from .generate import generate_project_audio
from .project import load_project
from .render import execute_render


def _stable_json(payload: Any) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2) + "\n"


class InterruptState:
    def __init__(self) -> None:
        self.requested = False
        self.previous_handler: Any = None

    def install(self) -> None:
        def handler(_signum: int, _frame: Any) -> None:
            self.requested = True

        self.previous_handler = signal.signal(signal.SIGINT, handler)

    def restore(self) -> None:
        if self.previous_handler is not None:
            signal.signal(signal.SIGINT, self.previous_handler)
            self.previous_handler = None


def _add_project_argument(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--project", required=True, help="project JSON path")


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="theatre-binaural",
        description="Deterministic offline binaural theatre master renderer",
    )
    subparsers = parser.add_subparsers(dest="command")

    generate = subparsers.add_parser("generate", help="generate the eight declared WAV tracks")
    _add_project_argument(generate)

    validate = subparsers.add_parser("validate", help="validate the project and all WAV headers")
    _add_project_argument(validate)

    render = subparsers.add_parser("render", help="render the binaural master with resumable snapshots")
    _add_project_argument(render)
    render.add_argument("-o", "--master", default="master.wav", help="output binaural WAV")
    render.add_argument("--report", default="render_report.json", help="stable JSON report")
    render.add_argument("--state-dir", default=".binaural_state", help="snapshot directory")
    render.add_argument("--fresh", action="store_true", help="discard any existing snapshot")
    render.add_argument(
        "--block-delay-ms",
        type=float,
        default=0.0,
        help="optional delay after each committed block (used for interruption tests)",
    )
    return parser


def _render_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="theatre-binaural",
        description="Render a binaural master (default command)",
    )
    _add_project_argument(parser)
    parser.add_argument("-o", "--master", default="master.wav")
    parser.add_argument("--report", default="render_report.json")
    parser.add_argument("--state-dir", default=".binaural_state")
    parser.add_argument("--fresh", action="store_true")
    parser.add_argument("--block-delay-ms", type=float, default=0.0)
    return parser


def _run(args: argparse.Namespace) -> tuple[int, dict[str, Any]]:
    if args.command == "generate":
        payload = generate_project_audio(args.project)
        return 0, payload
    if args.command == "validate":
        project = load_project(args.project, validate_audio=True)
        payload = {
            "status": "ok",
            "operation": "validate",
            "project_id": project.project_id,
            "sample_rate": project.sample_rate,
            "tracks": len(project.tracks),
            "head_tracking_samples": len(project.head_samples),
            "reference_channel": project.reference_channel,
            "block_frames": project.block_frames,
        }
        return 0, payload
    if args.command == "render":
        interrupt = InterruptState()
        interrupt.install()
        try:
            outcome = execute_render(
                args.project,
                args.master,
                args.report,
                args.state_dir,
                fresh=bool(args.fresh),
                interrupt_requested=lambda: interrupt.requested,
                block_delay_s=max(0.0, float(args.block_delay_ms) / 1000.0),
            )
        finally:
            interrupt.restore()
        return 0, outcome.report
    raise BusinessError("E_COMMAND", "cli", "no command selected", 2)


def main(argv: Sequence[str] | None = None) -> int:
    arguments = list(sys.argv[1:] if argv is None else argv)
    if arguments and arguments[0] in {"generate", "validate", "render"}:
        parser = _build_parser()
        args = parser.parse_args(arguments)
        if args.command is None:
            parser.print_help(sys.stderr)
            return 2
    elif arguments and arguments[0] not in {"-h", "--help"}:
        parser = _render_parser()
        args = parser.parse_args(arguments)
        args.command = "render"
    elif not arguments:
        _build_parser().print_help(sys.stderr)
        return 2
    else:
        _build_parser().parse_args(arguments)
        return 0

    try:
        code, payload = _run(args)
    except BusinessError as exc:
        sys.stderr.write(_stable_json(exc.as_dict()))
        return int(exc.exit_code)
    except BrokenPipeError:
        return 1
    except Exception as exc:
        error = BusinessError(
            "E_INTERNAL",
            "cli",
            f"{type(exc).__name__}: {exc}",
            EXIT_RENDER,
        )
        sys.stderr.write(_stable_json(error.as_dict()))
        return int(error.exit_code)
    sys.stdout.write(_stable_json(payload))
    return int(code)


if __name__ == "__main__":
    raise SystemExit(main())

