from __future__ import annotations

import argparse
import hashlib
import signal
import sys
import time
from pathlib import Path
from typing import Any

from .compare import compare_outputs
from .errors import SceneValidationError
from .geometry import build_geometry_context
from .pngio import atomic_write_bytes, encode_gray16, encode_rgb8
from .projection import build_projection_hits, build_shadow_profiles
from .raster import (
    ChunkResult,
    chunk_count,
    render_chunk,
)
from .report import (
    assemble_image,
    attribution_document,
    build_stats,
    stable_json_bytes,
)
from .sample import write_sample_scene
from .scene import Scene, load_scene
from .snapshot import load_snapshot, save_snapshot
from .texture import save_texture_tables

STOP_REQUESTED = False
INTERRUPT_REASON = ""


def _signal_handler(signum: int, frame: Any) -> None:
    global STOP_REQUESTED, INTERRUPT_REASON
    STOP_REQUESTED = True
    try:
        INTERRUPT_REASON = signal.Signals(signum).name.lower()
    except ValueError:
        INTERRUPT_REASON = f"signal_{signum}"


def _install_signal_handlers() -> dict[int, Any]:
    previous: dict[int, Any] = {}
    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            number = getattr(signal, name)
            try:
                previous[number] = signal.getsignal(number)
                signal.signal(number, _signal_handler)
            except (ValueError, OSError):
                pass
    return previous


def _restore_signal_handlers(previous: dict[int, Any]) -> None:
    for number, handler in previous.items():
        try:
            signal.signal(number, handler)
        except (ValueError, OSError):
            pass


def _digest_bytes(payload: bytes) -> str:
    return "sha256:" + hashlib.sha256(payload).hexdigest()


def _print_result(value: dict[str, Any]) -> None:
    sys.stdout.write(stable_json_bytes(value).decode("utf-8"))
    sys.stdout.flush()


def _write_json(path: Path, value: dict[str, Any]) -> bytes:
    payload = stable_json_bytes(value)
    atomic_write_bytes(path, payload)
    return payload


def command_sample(args: argparse.Namespace) -> int:
    output = Path(args.out)
    write_sample_scene(output, args.seed)
    _print_result(
        {
            "ok": True,
            "sample": output.name,
            "seed": int(args.seed),
        }
    )
    return 0


def command_validate(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    _print_result(
        {
            "ok": True,
            "scene_digest": scene.digest,
            "pane_count": len(scene.panes),
            "sector_ids": list(scene.sector_ids),
            "image_size": [scene.camera.width, scene.camera.height],
            "chunk_count": chunk_count(scene)[2],
        }
    )
    return 0


def _write_complete_outputs(
    scene: Scene,
    context: Any,
    output_dir: Path,
    results: dict[int, ChunkResult],
    recovery_count: int,
    snapshot_status: str,
    snapshot_reason: str | None,
    resumed_chunks: int,
) -> dict[str, Any]:
    rgb, labels = assemble_image(scene, results)
    color_payload = encode_rgb8(rgb)
    label_payload = encode_gray16(labels)
    atomic_write_bytes(output_dir / "color.png", color_payload)
    atomic_write_bytes(output_dir / "attribution.png", label_payload)
    _write_json(output_dir / "attribution.json", attribution_document(scene, context))
    save_texture_tables(output_dir / "textures.npy", context.textures, scene)
    projection_hits = build_projection_hits(context)
    projection_payload = _write_json(output_dir / "projection_hits.json", projection_hits)
    shadow_profiles = build_shadow_profiles(context)
    _write_json(output_dir / "shadow_profiles.json", shadow_profiles)
    file_digests = {
        "color.png": _digest_bytes(color_payload),
        "attribution.png": _digest_bytes(label_payload),
        "projection_hits.json": _digest_bytes(projection_payload),
    }
    stats = build_stats(
        scene,
        context,
        results,
        rgb,
        labels,
        projection_hits,
        shadow_profiles,
        file_digests,
        recovery_count,
        snapshot_status,
        snapshot_reason,
        resumed_chunks,
    )
    _write_json(output_dir / "stats.json", stats)
    recovery_report = output_dir / "recovery_report.json"
    if recovery_report.exists():
        recovery_report.unlink()
    return stats


def _prepare_outputs(
    output_dir: Path,
) -> None:
    output_dir.mkdir(parents=True, exist_ok=True)


def _clear_render_artifacts(output_dir: Path) -> None:
    for name in (
        "color.png",
        "attribution.png",
        "attribution.json",
        "projection_hits.json",
        "shadow_profiles.json",
        "textures.npy",
        "stats.json",
        "color_partial.png",
        "attribution_partial.png",
        "recovery_report.json",
        "differences.json",
    ):
        path = output_dir / name
        if path.exists():
            path.unlink()


def _write_interruption(
    output_dir: Path,
    scene: Scene,
    context: Any,
    results: dict[int, ChunkResult],
    recovery_count: int,
    reason: str,
) -> None:
    rgb, labels = assemble_image(scene, results)
    atomic_write_bytes(output_dir / "color_partial.png", encode_rgb8(rgb))
    atomic_write_bytes(output_dir / "attribution_partial.png", encode_gray16(labels))
    _write_json(output_dir / "attribution.json", attribution_document(scene, context))
    save_texture_tables(output_dir / "textures.npy", context.textures, scene)
    total = chunk_count(scene)[2]
    completed = sorted(results)
    report = {
        "schema_version": 1,
        "status": "interrupted",
        "reason": reason,
        "scene_digest": scene.digest,
        "chunk_size": list(scene.render.chunk_size),
        "completed_chunks": completed,
        "completed_count": len(completed),
        "next_chunk": completed[-1] + 1 if completed else 0,
        "total_chunks": total,
        "recovery_count": int(recovery_count),
        "resume_policy": "rerun the same render command; a valid snapshot is auto-detected",
    }
    _write_json(output_dir / "recovery_report.json", report)


def command_render(args: argparse.Namespace) -> int:
    global STOP_REQUESTED, INTERRUPT_REASON
    STOP_REQUESTED = False
    INTERRUPT_REASON = ""
    if args.stop_after_chunk is not None and args.stop_after_chunk < 1:
        raise ValueError("--stop-after-chunk must be at least 1")
    scene = load_scene(args.scene)
    context = build_geometry_context(scene)
    output_dir = Path(args.out)
    _prepare_outputs(output_dir)
    snapshot_path = Path(args.snapshot) if args.snapshot else output_dir / "snapshot.npz"
    if args.fresh:
        if snapshot_path.exists():
            snapshot_path.unlink()
        _clear_render_artifacts(output_dir)
        snapshot_status = "fresh"
        snapshot_reason = None
        loaded_results: dict[int, ChunkResult] = {}
        recovery_count = 0
    else:
        loaded = load_snapshot(snapshot_path, scene)
        snapshot_status = loaded.status
        snapshot_reason = loaded.discarded_reason
        loaded_results = loaded.results
        recovery_count = loaded.recovery_count
        if loaded.status == "valid" and loaded.results:
            recovery_count += 1
        elif loaded.status == "discarded":
            try:
                snapshot_path.unlink()
            except OSError:
                pass
            _clear_render_artifacts(output_dir)
            loaded_results = {}
            recovery_count = 0
            snapshot_status = "discarded_restart"
    resumed_chunks = len(loaded_results)
    results: dict[int, ChunkResult] = dict(loaded_results)
    columns, rows, total = chunk_count(scene)
    previous_handlers = _install_signal_handlers()
    try:
        for index in range(total):
            if index in results:
                continue
            result = render_chunk(context, index)
            results[index] = result
            save_snapshot(snapshot_path, scene, results, recovery_count)
            column = index % columns
            row = index // columns
            sys.stderr.write(
                f"committed chunk {index + 1}/{total} (grid {column},{row})\n"
            )
            sys.stderr.flush()
            if args.chunk_delay_ms > 0:
                time.sleep(args.chunk_delay_ms / 1000.0)
            stop_reason = None
            if STOP_REQUESTED:
                stop_reason = INTERRUPT_REASON or "signal"
            elif args.stop_after_chunk is not None and len(results) >= args.stop_after_chunk:
                stop_reason = "stop_after_chunk"
            if stop_reason and len(results) < total:
                _write_interruption(
                    output_dir, scene, context, results, recovery_count, stop_reason
                )
                _print_result(
                    {
                        "ok": True,
                        "status": "interrupted",
                        "reason": stop_reason,
                        "completed_chunks": sorted(results),
                        "completed_count": len(results),
                        "total_chunks": total,
                        "recovery_count": recovery_count,
                    }
                )
                return 0
        stats = _write_complete_outputs(
            scene,
            context,
            output_dir,
            results,
            recovery_count,
            snapshot_status,
            snapshot_reason,
            resumed_chunks,
        )
        save_snapshot(snapshot_path, scene, results, recovery_count)
        if args.reference:
            difference = compare_outputs(
                Path(args.reference),
                output_dir,
                scene,
                max_differences=args.max_differences,
            )
            difference_path = output_dir / "differences.json"
            _write_json(difference_path, difference)
            if not difference["ok"]:
                _print_result(difference)
                return 3
            difference_path.unlink()
        _print_result(
            {
                "ok": True,
                "status": "complete",
                "scene_digest": scene.digest,
                "stats": "stats.json",
                "recovery_count": recovery_count,
                "resumed_chunk_count": resumed_chunks,
                "chunks": stats["chunks"]["completed"],
            }
        )
        return 0
    finally:
        _restore_signal_handlers(previous_handlers)


def command_compare(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    result = compare_outputs(
        Path(args.reference),
        Path(args.candidate),
        scene,
        max_differences=args.max_differences,
    )
    if args.out:
        _write_json(Path(args.out), result)
    _print_result(result)
    return 0 if result["ok"] else 3


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="glass-verify",
        description="Deterministic offline stained-glass validation renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample_parser = subparsers.add_parser("sample", help="write the deterministic sample scene")
    sample_parser.add_argument("--out", required=True, help="output scene JSON")
    sample_parser.add_argument("--seed", type=int, default=20261118)
    sample_parser.set_defaults(handler=command_sample)

    validate_parser = subparsers.add_parser("validate", help="validate a scene JSON")
    validate_parser.add_argument("--scene", required=True)
    validate_parser.set_defaults(handler=command_validate)

    render_parser = subparsers.add_parser("render", help="render, snapshot and resume")
    render_parser.add_argument("--scene", required=True)
    render_parser.add_argument("--out", required=True)
    render_parser.add_argument("--snapshot", default=None)
    render_parser.add_argument("--fresh", action="store_true")
    render_parser.add_argument("--stop-after-chunk", type=int, default=None)
    render_parser.add_argument(
        "--chunk-delay-ms",
        type=int,
        default=0,
        help="test hook: wait after each committed chunk without changing pixels",
    )
    render_parser.add_argument(
        "--reference",
        default=None,
        help="optional uninterrupted output directory for byte-level recovery verification",
    )
    render_parser.add_argument("--max-differences", type=int, default=64)
    render_parser.set_defaults(handler=command_render)

    compare_parser = subparsers.add_parser("compare", help="compare two render outputs")
    compare_parser.add_argument("--scene", required=True)
    compare_parser.add_argument("--reference", required=True)
    compare_parser.add_argument("--candidate", required=True)
    compare_parser.add_argument("--out", default=None)
    compare_parser.add_argument("--max-differences", type=int, default=64)
    compare_parser.set_defaults(handler=command_compare)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except SceneValidationError as exc:
        _print_result(exc.as_dict())
        return 2
    except (OSError, ValueError) as exc:
        _print_result(
            {
                "ok": False,
                "error_kind": "runtime_error",
                "reason": str(exc),
            }
        )
        return 4


if __name__ == "__main__":
    raise SystemExit(main())
