from __future__ import annotations

import argparse
import signal
import sys
import time
from pathlib import Path
from typing import Sequence

from .errors import SceneValidationError
from .generator import generate_scene, write_scene
from .pipeline import render_state_fresh
from .render import RenderEngine
from .report import (
    build_resume_report,
    build_success_report,
    build_validation_report,
    compare_resume_outputs,
    write_json_report,
)
from .scene import load_scene, validate_scene_data
from .snapshot import load_snapshot, save_snapshot
from .state import apply_tile, build_output_artifacts
from .texture import write_texture_bundle
from .util import atomic_write_bytes, sha256_bytes


def _write_issues(path: Path, error: SceneValidationError) -> None:
    write_json_report(
        path,
        build_validation_report([issue.as_dict() for issue in error.issues]),
    )


def _write_runtime_error(path: Path, exc: BaseException) -> None:
    write_json_report(
        path,
        {
            "status": "error",
            "exit_code": 1,
            "code": "RENDER_FAILED",
            "image_written": False,
            "reason": exc.__class__.__name__,
        },
    )


def _generate_command(args: argparse.Namespace) -> int:
    scene = generate_scene(
        seed=args.seed,
        width=args.width,
        height=args.height,
        tile_width=args.tile_width,
        tile_height=args.tile_height,
        layer_count=args.layers,
        shadow_samples=args.shadow_samples,
        softness=args.softness,
    )
    validated = validate_scene_data(scene)
    digest = write_scene(scene, args.output)
    print(f"scene={Path(args.output).name}")
    print(f"scene_digest={digest}")
    print(f"scene_sha256={sha256_bytes(Path(args.output).read_bytes())}")
    if args.emit_textures is not None:
        manifest = write_texture_bundle(
            args.emit_textures,
            validated.seed,
            [(layer.layer_index, layer.texture_resolution) for layer in validated.layers],
        )
        print(f"texture_bundle_sha256={manifest['bundle_sha256']}")
    return 0


def _validate_command(args: argparse.Namespace) -> int:
    report_path = (
        Path(args.report)
        if args.report is not None
        else Path(args.scene).with_name("validation_report.json")
    )
    try:
        scene = load_scene(args.scene)
    except SceneValidationError as exc:
        _write_issues(report_path, exc)
        print(f"validation_failed={len(exc.issues)}", file=sys.stderr)
        return 2
    write_json_report(
        report_path,
        {
            "status": "ok",
            "exit_code": 0,
            "scene_digest": scene.digest,
            "image_written": False,
        },
    )
    print(f"scene_digest={scene.digest}")
    return 0


def _install_signal_handlers() -> tuple[dict, dict]:
    interrupted = {"value": False}
    previous: dict = {}

    def handler(signum, frame):
        del signum, frame
        interrupted["value"] = True

    for signal_name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, signal_name):
            signal_number = getattr(signal, signal_name)
            try:
                previous[signal_number] = signal.getsignal(signal_number)
                signal.signal(signal_number, handler)
            except (OSError, ValueError):
                pass
    return interrupted, previous


def _restore_signal_handlers(previous: dict) -> None:
    for signal_number, old_handler in previous.items():
        try:
            signal.signal(signal_number, old_handler)
        except (OSError, ValueError):
            pass


def _remove_snapshot(path: Path) -> None:
    try:
        path.unlink()
    except FileNotFoundError:
        pass


def _clean_stale_snapshot_temps(output_dir: Path) -> None:
    for candidate in output_dir.glob(".snapshot.bin.tmp-*"):
        try:
            candidate.unlink()
        except OSError:
            pass


def _render_command(args: argparse.Namespace) -> int:
    output_dir = Path(args.output_dir)
    report_path = output_dir / args.report
    snapshot_path = output_dir / "snapshot.bin"
    resume_report_path = output_dir / args.resume_report
    difference_report_path = output_dir / "difference_report.json"
    output_dir.mkdir(parents=True, exist_ok=True)
    _clean_stale_snapshot_temps(output_dir)
    try:
        scene = load_scene(args.scene)
    except SceneValidationError as exc:
        _write_issues(report_path, exc)
        print(f"validation_failed={len(exc.issues)}", file=sys.stderr)
        return 2

    if args.fresh:
        _remove_snapshot(snapshot_path)
        loaded = load_snapshot(snapshot_path, scene)
    else:
        loaded = load_snapshot(snapshot_path, scene)
    state = loaded.state
    snapshot_status = loaded.status
    if loaded.status == "resumed":
        state.recovery_count += 1
        save_snapshot(snapshot_path, scene, state)

    engine = RenderEngine(scene)
    interrupted, previous_handlers = _install_signal_handlers()
    newly_rendered = 0
    try:
        if (
            args.stop_after_blocks is not None
            and args.stop_after_blocks <= 0
            and len(state.completed_blocks) < scene.tile_count
        ):
            save_snapshot(snapshot_path, scene, state)
            write_json_report(resume_report_path, build_resume_report(scene, state))
            print(f"interrupted_completed={len(state.completed_blocks)}")
            return 0
        for block in range(len(state.completed_blocks), scene.tile_count):
            column = block % scene.tile_columns
            row = block // scene.tile_columns
            tile = engine.render_tile(block, column, row)
            apply_tile(state, scene, block, column, row, tile)
            save_snapshot(snapshot_path, scene, state)
            newly_rendered += 1
            if args.block_delay_ms > 0:
                time.sleep(args.block_delay_ms / 1000.0)
            should_stop = interrupted["value"] or (
                args.stop_after_blocks is not None
                and newly_rendered >= args.stop_after_blocks
                and len(state.completed_blocks) < scene.tile_count
            )
            if should_stop:
                write_json_report(resume_report_path, build_resume_report(scene, state))
                print(f"interrupted_completed={len(state.completed_blocks)}")
                return 0
    finally:
        _restore_signal_handlers(previous_handlers)

    artifacts = build_output_artifacts(scene, state)
    if loaded.status == "resumed":
        reference_state = render_state_fresh(scene)
        reference_artifacts = build_output_artifacts(scene, reference_state)
        consistent, difference_report = compare_resume_outputs(
            scene, artifacts, reference_artifacts
        )
        if not consistent:
            write_json_report(difference_report_path, difference_report)
            print(
                f"resume_mismatch={difference_report['difference_count']}",
                file=sys.stderr,
            )
            return 4

    for relative_path, payload in sorted(artifacts.payloads.items()):
        atomic_write_bytes(output_dir / relative_path, payload)
    write_texture_bundle(
        output_dir,
        scene.seed,
        [(layer.layer_index, layer.texture_resolution) for layer in scene.layers],
    )
    report = build_success_report(scene, state, artifacts, snapshot_status)
    write_json_report(report_path, report)
    _remove_snapshot(snapshot_path)
    try:
        resume_report_path.unlink(missing_ok=True)
    except OSError:
        pass
    print(f"scene_digest={scene.digest}")
    print(f"image_sha256={report['image']['rgba_sha256']}")
    print(f"depth_sha256={report['image']['depth_sha256']}")
    print(f"recovery_count={state.recovery_count}")
    return 0


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="paper-lantern",
        description="Deterministic offline soft-raster paper-lantern validator",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser(
        "generate", help="write the deterministic five-layer demo scene"
    )
    generate_parser.add_argument("--output", default="scene.json")
    generate_parser.add_argument("--seed", type=int, default=206)
    generate_parser.add_argument("--width", type=int, default=192)
    generate_parser.add_argument("--height", type=int, default=144)
    generate_parser.add_argument("--tile-width", type=int, default=32)
    generate_parser.add_argument("--tile-height", type=int, default=24)
    generate_parser.add_argument("--layers", type=int, default=5)
    generate_parser.add_argument("--shadow-samples", type=int, default=16)
    generate_parser.add_argument("--softness", type=float, default=0.045)
    generate_parser.add_argument("--emit-textures", default=None)
    generate_parser.set_defaults(handler=_generate_command)

    validate_parser = subparsers.add_parser(
        "validate", help="validate a scene and write a structured report"
    )
    validate_parser.add_argument("scene")
    validate_parser.add_argument("--report", default=None)
    validate_parser.set_defaults(handler=_validate_command)

    render_parser = subparsers.add_parser(
        "render", help="render a scene with atomic tile snapshots"
    )
    render_parser.add_argument("scene")
    render_parser.add_argument("-o", "--output-dir", default="render_out")
    render_parser.add_argument("--report", default="report.json")
    render_parser.add_argument("--resume-report", default="resume_report.json")
    render_parser.add_argument("--fresh", action="store_true")
    render_parser.add_argument("--stop-after-blocks", type=int, default=None)
    render_parser.add_argument("--block-delay-ms", type=float, default=0.0)
    render_parser.set_defaults(handler=_render_command)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    output_dir = Path(getattr(args, "output_dir", "."))
    report_path = output_dir / getattr(args, "report", "report.json")
    if args.command == "render" and getattr(args, "report", None) is None:
        report_path = output_dir / "report.json"
    try:
        return int(args.handler(args))
    except SceneValidationError as exc:
        if args.command == "render":
            report = report_path
        elif args.command == "generate":
            report = Path(args.output).with_name("validation_report.json")
        else:
            report = Path(args.report or "validation_report.json")
        report.parent.mkdir(parents=True, exist_ok=True)
        _write_issues(report, exc)
        print(f"validation_failed={len(exc.issues)}", file=sys.stderr)
        return 2
    except Exception as exc:
        report.parent.mkdir(parents=True, exist_ok=True)
        _write_runtime_error(report, exc)
        print(f"render_failed={exc.__class__.__name__}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

