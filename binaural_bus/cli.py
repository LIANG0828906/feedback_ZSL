"""Command-line interface for deterministic generation and binaural synthesis."""

from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Sequence

from .errors import BinauralBusError
from .generate import write_generated_material
from .project import load_project
from .render import InterruptFlag, synthesize


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="binaural-bus",
        description="Deterministic offline binaural bus renderer for live-drama multitrack WAVs.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="Generate deterministic actor and speaker WAV material."
    )
    generate.add_argument("--project", required=True, help="Project JSON path.")
    generate.add_argument(
        "--out",
        default="inputs",
        help="Directory for generated WAV channels (default: inputs).",
    )
    generate.add_argument(
        "--no-truth",
        action="store_true",
        help="Do not write diagnostic direct-only actor tracks.",
    )

    validate = subparsers.add_parser(
        "validate", help="Validate project schema and all declared channel files."
    )
    validate.add_argument("--project", required=True, help="Project JSON path.")
    validate.add_argument(
        "--audio-dir",
        default=None,
        help="Directory containing channel files (default: project directory).",
    )

    render = subparsers.add_parser(
        "synthesize", help="Render the binaural master with resumable chunk export."
    )
    render.add_argument("--project", required=True, help="Project JSON path.")
    render.add_argument(
        "--audio-dir",
        default=None,
        help="Directory containing channel files (default: project directory).",
    )
    render.add_argument(
        "--output",
        default="output",
        help="Output directory (default: output).",
    )
    render.add_argument(
        "--fresh",
        action="store_true",
        help="Discard existing master, report, and snapshots before rendering.",
    )
    render.add_argument(
        "--no-resume",
        action="store_true",
        help="Do not read prior snapshots; start at block zero.",
    )
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _generate(args)
        if args.command == "validate":
            return _validate(args)
        if args.command == "synthesize":
            return _synthesize(args)
    except BinauralBusError as exc:
        print(json.dumps(exc.payload(), ensure_ascii=True, indent=2))
        return exc.exit_code
    except KeyboardInterrupt:
        print(
            json.dumps(
                {
                    "status": "interrupted",
                    "code": "E_INTERRUPTED_BEFORE_COMMIT",
                    "exit_code": 0,
                },
                ensure_ascii=True,
                indent=2,
            )
        )
        return 0
    return 1


def _generate(args: argparse.Namespace) -> int:
    project = load_project(args.project, Path(args.project).parent, validate_files=False)
    material = write_generated_material(
        project, args.out, write_truth=not args.no_truth
    )
    result = {
        "status": "ok",
        "command": "generate",
        "project_id": project.project_id,
        "output_directory": _display_path(args.out),
        "actors": [actor.id for actor in project.actors],
        "speakers": [speaker.id for speaker in project.speakers],
        "crosstalk_paths": len(material.injected_paths),
        "project_digest": project.content_digest(),
    }
    print(json.dumps(result, ensure_ascii=True, indent=2))
    return 0


def _validate(args: argparse.Namespace) -> int:
    project = load_project(
        args.project,
        args.audio_dir if args.audio_dir is not None else Path(args.project).parent,
        validate_files=True,
    )
    result = {
        "status": "ok",
        "command": "validate",
        "project_id": project.project_id,
        "sample_rate": project.sample_rate,
        "bit_depth": project.bit_depth,
        "channels_checked": len(project.actors) + len(project.speakers),
        "actors": len(project.actors),
        "speakers": len(project.speakers),
        "project_digest": project.content_digest(),
    }
    print(json.dumps(result, ensure_ascii=True, indent=2))
    return 0


def _synthesize(args: argparse.Namespace) -> int:
    interrupt_flag = InterruptFlag()
    previous_handler = None
    try:
        previous_handler = signal.getsignal(signal.SIGINT)

        def request_graceful_interrupt(_signum, _frame) -> None:
            interrupt_flag.requested = True

        signal.signal(signal.SIGINT, request_graceful_interrupt)
    except (ValueError, OSError):
        previous_handler = None

    try:
        result = synthesize(
            args.project,
            args.audio_dir if args.audio_dir is not None else Path(args.project).parent,
            args.output,
            resume=not args.no_resume,
            fresh=args.fresh,
            interrupt_flag=interrupt_flag,
        )
    finally:
        if previous_handler is not None:
            try:
                signal.signal(signal.SIGINT, previous_handler)
            except (ValueError, OSError):
                pass

    output = {
        "status": result.status,
        "command": "synthesize",
        "project_id": result.project.project_id,
        "master_file": (
            _display_path(result.master_path) if result.master_path is not None else None
        ),
        "report_file": (
            _display_path(result.report_path) if result.report_path is not None else None
        ),
        "recovery_file": (
            _display_path(result.recovery_path)
            if result.recovery_path is not None
            else None
        ),
        "resume_code": result.resume_code,
    }
    print(json.dumps(output, ensure_ascii=True, indent=2))
    return 0


def _display_path(value: str | Path) -> str:
    path = Path(value)
    try:
        return str(path.resolve().relative_to(Path.cwd().resolve())).replace("\\", "/")
    except ValueError:
        return path.name


if __name__ == "__main__":
    sys.exit(main())

