"""Command-line entry points for inspection, verification and export."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .checkpoint import CheckpointStore
from .errors import PipelineError
from .export import export_bundle
from .loader import load_scene
from .renderer import render_all_frames
from .session import CompositorSession
from .verification import run_verification


def _default_samples() -> Path:
    return Path.cwd() / "samples"


def _print_json(value: Any) -> None:
    print(json.dumps(value, sort_keys=True, indent=2, ensure_ascii=False))


def _command_inspect(args: argparse.Namespace) -> int:
    model = load_scene(
        Path(args.samples) / f"scene_vpfc{args.source_version}.json"
    )
    payload = {
        "scene_id": model.scene_id,
        "source_version": model.source_version,
        "canvas": model.canvas.to_dict(),
        "timeline": model.timeline.to_dict(),
        "layers": [
            {
                "id": layer.layer_id,
                "kind": layer.kind,
                "parent": layer.parent_id,
                "stack_order": layer.stack_order,
                "alpha_mode": layer.alpha_mode,
                "source_color_space": layer.source_color_space,
                "visible_from": layer.visible_from,
                "visible_to": layer.visible_to,
            }
            for layer in model.layers
        ],
        "mapping_entries": len(model.mapping_trace),
    }
    _print_json(payload)
    return 0


def _command_run(args: argparse.Namespace) -> int:
    model = load_scene(
        Path(args.samples) / f"scene_vpfc{args.source_version}.json"
    )
    session = CompositorSession(
        model,
        CheckpointStore(args.checkpoints),
        max_workers=args.workers,
        max_frame_buffers_per_thread=args.frame_buffers,
    )
    result = session.run(
        parallel=not args.serial,
        max_workers=args.workers,
    )
    comparison = session.compare_with_full(raise_on_diff=False)
    payload = {
        "progress": result.progress_label,
        "commit_order": list(result.commit_order),
        "reused_windows": list(result.reused_windows),
        "failed_windows": list(result.failed_windows),
        "invalidations": [item.to_dict() for item in result.invalidations],
        "full_comparison_ok": comparison.ok,
        "first_difference": (
            comparison.report.to_dict() if comparison.report else None
        ),
        "recovery": {
            "discarded": session.recovery.discarded,
            "restored_windows": sorted(session.recovery.restored),
            "reasons": list(session.recovery.reasons),
        },
    }
    _print_json(payload)
    return 0 if comparison.ok and not result.failed_windows else 2


def _command_export(args: argparse.Namespace) -> int:
    model = load_scene(
        Path(args.samples) / f"scene_vpfc{args.source_version}.json"
    )
    frames = render_all_frames(model)
    result = export_bundle(model, frames, args.target, args.output)
    _print_json(
        {
            "target_version": result.target_version,
            "output": str(result.output_directory),
            "scene_sha256": result.scene_sha256,
            "frames_sha256": result.frames_sha256,
            "warning_codes": list(result.warning_codes),
            "warnings": list(result.warnings),
            "degradation_trace": list(result.degradation_trace),
        }
    )
    return 0


def _command_verify(args: argparse.Namespace) -> int:
    payload = run_verification(args.samples)
    _print_json(payload)
    return 0


def _command_gui(args: argparse.Namespace) -> int:
    from .ui import run_ui

    return run_ui(
        args.samples,
        args.checkpoints,
        source_version=args.source_version,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="vp-pipeline",
        description="Deterministic virtual-production compositing pipeline",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    inspect_parser = subparsers.add_parser("inspect", help="Inspect one scene")
    inspect_parser.add_argument("--samples", default=str(_default_samples()))
    inspect_parser.add_argument("--source-version", type=int, choices=(1, 2, 3), default=3)
    inspect_parser.set_defaults(handler=_command_inspect)

    run_parser = subparsers.add_parser("run", help="Run windowed composition")
    run_parser.add_argument("--samples", default=str(_default_samples()))
    run_parser.add_argument("--checkpoints", default="artifacts/checkpoints")
    run_parser.add_argument("--source-version", type=int, choices=(1, 2, 3), default=3)
    run_parser.add_argument("--workers", type=int, default=2)
    run_parser.add_argument("--frame-buffers", type=int, default=1)
    run_parser.add_argument("--serial", action="store_true")
    run_parser.set_defaults(handler=_command_run)

    export_parser = subparsers.add_parser("export", help="Export scene and frames")
    export_parser.add_argument("--samples", default=str(_default_samples()))
    export_parser.add_argument("--source-version", type=int, choices=(1, 2, 3), default=3)
    export_parser.add_argument("--target", choices=("vpfc/1", "vpfc/2", "vpfc/3"), required=True)
    export_parser.add_argument("--output", required=True)
    export_parser.set_defaults(handler=_command_export)

    verify_parser = subparsers.add_parser(
        "verify", help="Run local end-to-end acceptance checks"
    )
    verify_parser.add_argument("--samples", default=str(_default_samples()))
    verify_parser.set_defaults(handler=_command_verify)

    gui_parser = subparsers.add_parser("gui", help="Launch the PySide6 operator UI")
    gui_parser.add_argument("--samples", default=str(_default_samples()))
    gui_parser.add_argument("--checkpoints", default="artifacts/checkpoints")
    gui_parser.add_argument("--source-version", type=int, choices=(1, 2, 3), default=3)
    gui_parser.set_defaults(handler=_command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except PipelineError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
