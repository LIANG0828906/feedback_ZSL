from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any, Sequence

from .demo import demo_manifest
from .errors import E_RENDER_DIFFERENCE, E_RESUME_MISMATCH, BusinessError, ResumeMismatch
from .manifest import load_manifest
from .render import Renderer
from .synthesis import synthesize_project, write_json_atomic
from .verify import differences_exit_payload, verify_outputs, write_differences


class InterruptFlag:
    def __init__(self) -> None:
        self.requested = False

    def __call__(self) -> bool:
        return self.requested

    def handler(self, signum, frame) -> None:  # noqa: ANN001
        self.requested = True
        print(
            json.dumps(
                {
                    "code": "INTERRUPT_RECEIVED",
                    "message": "finishing the current block and committing a snapshot",
                },
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="rainforest-master",
        description="Deterministic 48 kHz rainforest ambience loop mastering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    make_demo = subparsers.add_parser("make-demo", help="write a deterministic demo manifest")
    make_demo.add_argument("--out", required=True, help="project directory")
    make_demo.add_argument("--seed", type=int, default=249)
    make_demo.add_argument("--duration", type=float, default=600.0)
    make_demo.add_argument("--block-frames", type=int, default=48_000)

    synth = subparsers.add_parser("synth", help="synthesize all seven mono stems")
    synth.add_argument("--manifest", required=True)
    synth.add_argument("--report", default=None)

    render = subparsers.add_parser("render", help="render the loop master block by block")
    render.add_argument("--manifest", required=True)
    render.add_argument("--out-dir", required=True)
    render.add_argument("--state-dir", default=None)
    render.add_argument("--stop-after-blocks", type=int, default=None, help=argparse.SUPPRESS)
    render.add_argument("--verify-reference", default=None)
    render.add_argument("--quiet", action="store_true")

    verify = subparsers.add_parser("verify", help="compare two rendered output directories")
    verify.add_argument("--reference", required=True)
    verify.add_argument("--candidate", required=True)
    verify.add_argument("--block-frames", type=int, default=48_000)
    verify.add_argument("--output", default=None)

    demo = subparsers.add_parser("demo", help="generate, synthesize and render in one command")
    demo.add_argument("--out", required=True)
    demo.add_argument("--seed", type=int, default=249)
    demo.add_argument("--duration", type=float, default=600.0)
    demo.add_argument("--block-frames", type=int, default=48_000)
    demo.add_argument("--quiet", action="store_true")
    return parser


def _emit(payload: dict[str, Any], *, stream=sys.stdout) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True), file=stream)


def _command_make_demo(args: argparse.Namespace) -> int:
    path = demo_manifest(
        args.out,
        seed=args.seed,
        duration_seconds=args.duration,
        block_frames=args.block_frames,
    )
    _emit({"code": "MANIFEST_WRITTEN", "manifest": str(path)})
    return 0


def _command_synth(args: argparse.Namespace) -> int:
    report = synthesize_project(args.manifest)
    if args.report:
        write_json_atomic(args.report, report)
    _emit(report)
    return 0


def _command_render(args: argparse.Namespace) -> int:
    interrupt = InterruptFlag()
    previous = signal.getsignal(signal.SIGINT)
    signal.signal(signal.SIGINT, interrupt.handler)
    try:
        manifest = load_manifest(args.manifest)
        with Renderer(manifest, args.out_dir, args.state_dir) as renderer:
            def progress() -> bool:
                if not args.quiet and renderer.completed_blocks:
                    print(
                        json.dumps(
                            {
                                "code": "RENDER_PROGRESS",
                                "completed_blocks": renderer.completed_blocks,
                                "total_blocks": renderer.total_blocks,
                            },
                            ensure_ascii=False,
                        ),
                        file=sys.stderr,
                    )
                return interrupt.requested

            outcome = renderer.run(
                stop_requested=progress,
                stop_after_blocks=args.stop_after_blocks,
            )
        if outcome.interrupted:
            _emit(
                {
                    "code": "CHECKPOINT_COMMITTED",
                    "status": "INTERRUPTED",
                    "completed_blocks": outcome.completed_blocks,
                    "total_blocks": outcome.total_blocks,
                    "recovery_report": str(
                        Path(args.out_dir) / "recovery_report.json"
                    ),
                }
            )
            return 0
        assert outcome.report is not None
        if args.verify_reference:
            differences = verify_outputs(
                args.verify_reference,
                args.out_dir,
                block_frames=manifest.rendering.block_frames,
            )
            if differences:
                write_differences(Path(args.out_dir) / "render_differences.json", differences)
                _emit(differences_exit_payload(differences), stream=sys.stderr)
                return 4
        summary = {
            "code": "RENDER_COMPLETE",
            "status": outcome.report["status"],
            "report": str(outcome.report_path),
            "stereo": str(Path(args.out_dir) / "rainforest_stereo_master.wav"),
            "mono": str(Path(args.out_dir) / "rainforest_mono_master.wav"),
            "checks": outcome.report["checks"],
        }
        _emit(summary)
        return 0 if outcome.report["status"] == "PASS" else 5
    finally:
        signal.signal(signal.SIGINT, previous)


def _command_verify(args: argparse.Namespace) -> int:
    differences = verify_outputs(
        args.reference,
        args.candidate,
        block_frames=args.block_frames,
    )
    if args.output:
        write_differences(args.output, differences)
    if differences:
        _emit(differences_exit_payload(differences), stream=sys.stderr)
        return 4
    _emit(
        {
            "code": "OUTPUTS_IDENTICAL",
            "stereo": True,
            "mono": True,
            "report": True,
        }
    )
    return 0


def _command_demo(args: argparse.Namespace) -> int:
    manifest_path = demo_manifest(
        args.out,
        seed=args.seed,
        duration_seconds=args.duration,
        block_frames=args.block_frames,
    )
    synth_report = synthesize_project(manifest_path)
    write_json_atomic(Path(args.out) / "synthesis_report.json", synth_report)
    if not args.quiet:
        _emit(synth_report)
    render_args = argparse.Namespace(
        manifest=str(manifest_path),
        out_dir=str(Path(args.out) / "master"),
        state_dir=None,
        stop_after_blocks=None,
        verify_reference=None,
        quiet=args.quiet,
    )
    return _command_render(render_args)


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "make-demo":
            return _command_make_demo(args)
        if args.command == "synth":
            return _command_synth(args)
        if args.command == "render":
            return _command_render(args)
        if args.command == "verify":
            return _command_verify(args)
        if args.command == "demo":
            return _command_demo(args)
        parser.error("unknown command")
        return 2
    except BusinessError as exc:
        _emit(exc.as_dict(), stream=sys.stderr)
        return 2
    except ResumeMismatch as exc:
        _emit(
            {
                "code": E_RESUME_MISMATCH,
                "message": str(exc),
                "old_progress_discarded": True,
            },
            stream=sys.stderr,
        )
        return 3
    except KeyboardInterrupt:
        _emit(
            {
                "code": "INTERRUPT_UNHANDLED",
                "message": "interrupted outside a committed block boundary",
            },
            stream=sys.stderr,
        )
        return 130
    except Exception as exc:
        _emit(
            {
                "code": "E_INTERNAL",
                "message": f"{type(exc).__name__}: {exc}",
            },
            stream=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

