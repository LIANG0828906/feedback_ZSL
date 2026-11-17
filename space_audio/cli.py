"""Command line interface for the offline spatial audio renderer."""

from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path

from .demo import write_demo_scene
from .errors import BusinessError, GRACEFUL_INTERRUPT, RESUME_MISMATCH
from .report import assert_masters_match, build_report, write_report
from .renderer import SoundscapeRenderer
from .scene import load_scene, validate_scene
from .synthesis import generate_all_dry_sources


def _emit_error(error: BusinessError) -> int:
    sys.stdout.write(json.dumps({"ok": False, "errors": [error.to_dict()]}, indent=2) + "\n")
    return 2


def _emit_errors(errors: list[BusinessError]) -> int:
    sys.stdout.write(json.dumps({"ok": False, "errors": [e.to_dict() for e in errors]}, indent=2) + "\n")
    return 2


def cmd_generate(args: argparse.Namespace) -> int:
    scene_path = write_demo_scene(args.scene)
    scene = json.loads(Path(args.scene).read_text(encoding="utf-8"))
    written = generate_all_dry_sources(scene, Path(args.scene).parent)
    sys.stdout.write(json.dumps({
        "ok": True,
        "scene": str(scene_path),
        "dry_files": sorted(str(path) for path in written.values()),
    }, indent=2) + "\n")
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    scene_path = Path(args.scene)
    base_dir = scene_path.parent
    try:
        scene = load_scene(scene_path)
    except BusinessError as error:
        return _emit_error(error)

    if not args.skip_generation:
        try:
            generate_all_dry_sources(scene, base_dir)
        except BusinessError as error:
            return _emit_error(error)

    errors = validate_scene(scene, base_dir=base_dir)
    if errors:
        return _emit_errors(errors)

    work_dir = Path(args.work_dir)
    renderer = SoundscapeRenderer(scene, base_dir, work_dir)
    renderer.kill_after_block = args.kill_after_block

    def handle_interrupt(signum, frame):  # noqa: ANN001
        renderer.interrupt_requested = True

    # Defer KeyboardInterrupt: the flag is observed only at block boundaries,
    # so the current block always finishes and its snapshot is committed.
    previous_int = signal.signal(signal.SIGINT, handle_interrupt)
    signal.siginterrupt(signal.SIGINT, False)
    previous_term = getattr(signal, "SIGTERM", None)
    if previous_term is not None:
        signal.signal(signal.SIGTERM, handle_interrupt)
        signal.siginterrupt(signal.SIGTERM, False)
    try:
        result = renderer.render()
    except BusinessError as error:
        return _emit_error(error)
    finally:
        try:
            signal.signal(signal.SIGINT, signal.SIG_DFL if not callable(previous_int) else previous_int)
            if previous_term is not None:
                signal.signal(signal.SIGTERM, signal.SIG_DFL if not callable(previous_term) else previous_term)
        except (ValueError, TypeError, OSError):
            pass

    if result.graceful_interrupt:
        committed = len(list(work_dir.glob("block_*.raw")))
        recovery_report = {
            "code": GRACEFUL_INTERRUPT,
            "completed_blocks": int(committed),
            "total_blocks": result.block_count,
            "resume_count": result.resume_count,
            "snapshot": "snapshot.npz",
        }
        write_report(recovery_report, work_dir / "recovery_report.json")
        if result.resume_mismatch:
            mismatch = {
                "code": RESUME_MISMATCH,
                "field_path": "$.snapshot.scene_digest",
                "message": "stale snapshot discarded; renderer restarted from block 0",
            }
            write_report(mismatch, work_dir / "resume_mismatch.json")
        output = {
            "ok": True,
            "interrupted": True,
            "recovery_report": str(work_dir / "recovery_report.json"),
            "completed_blocks": committed,
            "block_count": result.block_count,
            "resume_count": result.resume_count,
            "resume_mismatch": result.resume_mismatch,
        }
        sys.stdout.write(json.dumps(output, indent=2) + "\n")
        return 0

    report = build_report(scene, result)
    report_path = work_dir / "report.json"
    write_report(report, report_path)
    recovery_report = None
    if result.resume_mismatch:
        mismatch = {
            "code": RESUME_MISMATCH,
            "field_path": "$.snapshot.scene_digest",
            "message": "stale snapshot discarded; renderer restarted from block 0",
        }
        write_report(mismatch, work_dir / "resume_mismatch.json")
    output = {
        "ok": True,
        "master": str(result.master_path),
        "report": str(report_path),
        "total_samples": result.total_samples,
        "block_count": result.block_count,
        "resume_count": result.resume_count,
        "graceful_interrupt": result.graceful_interrupt,
        "resume_mismatch": result.resume_mismatch,
    }
    if recovery_report is not None:
        output["recovery_report"] = recovery_report
    sys.stdout.write(json.dumps(output, indent=2) + "\n")
    return 0


def cmd_compare(args: argparse.Namespace) -> int:
    try:
        comparison = assert_masters_match(args.reference, args.candidate)
    except BusinessError as error:
        payload = json.loads(json.dumps(error.to_dict()))
        sys.stdout.write(json.dumps({"ok": False, **payload}, indent=2) + "\n")
        return 3
    sys.stdout.write(json.dumps({"ok": True, "comparison": comparison}, indent=2) + "\n")
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    errors = validate_scene(scene, base_dir=Path(args.scene).parent)
    if errors:
        return _emit_errors(errors)
    report_path = Path(args.report)
    report = json.loads(report_path.read_text(encoding="utf-8"))
    issues = []
    expected_sources = [source["id"] for source in scene["sources"]]
    for frame in report["frames"]:
        ids = [entry["id"] for entry in frame["sources"]]
        if ids != expected_sources:
            issues.append({"frame": frame["frame"], "field": "source_ids", "expected": expected_sources,
                           "actual": ids})
            break
        for entry in frame["sources"]:
            if not (0.0 <= entry["transmission"] <= 1.0):
                issues.append({"frame": frame["frame"], "field": "transmission", "id": entry["id"]})
            if entry["occlusion_count"] != len(entry["occluded_layers"]):
                issues.append({"frame": frame["frame"], "field": "occlusion_count", "id": entry["id"]})
    if issues:
        sys.stdout.write(json.dumps({"ok": False, "issues": issues}, indent=2) + "\n")
        return 4
    sys.stdout.write(json.dumps({"ok": True, "frames": len(report["frames"]),
                                 "master_sha256": report["master"]["sha256"]}, indent=2) + "\n")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Deterministic offline spatial soundscape renderer")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="generate deterministic demo scene and dry WAVs")
    gen.add_argument("--scene", default="demo/scene.json")
    gen.set_defaults(func=cmd_generate)

    render = sub.add_parser("render", help="render a scene into a stereo master")
    render.add_argument("--scene", required=True)
    render.add_argument("--work-dir", required=True)
    render.add_argument("--kill-after-block", type=int, default=None,
                        help="simulate a hard kill (exit 99) after N committed blocks")
    render.add_argument("--skip-generation", action="store_true",
                        help="do not regenerate dry WAV files")
    render.set_defaults(func=cmd_render)

    compare = sub.add_parser("compare", help="byte-compare two master WAV files")
    compare.add_argument("--reference", required=True)
    compare.add_argument("--candidate", required=True)
    compare.set_defaults(func=cmd_compare)

    verify = sub.add_parser("verify", help="validate a scene and cross-check a report structure")
    verify.add_argument("--scene", required=True)
    verify.add_argument("--report", required=True)
    verify.set_defaults(func=cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())

