"""Command-line entry points for generation, rendering, and table export."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np

from .checkpoint import load_snapshot, remove_snapshot, save_snapshot
from .fileio import (
    png_gray16_bytes,
    png_rgba_bytes,
    write_json_atomic,
    write_npy_atomic,
    write_png_atomic,
)
from .optics import build_film_tables
from .render import (
    build_context,
    enumerate_blocks,
    hue_continuity,
    render_block,
)
from .report import build_report, make_block_record
from .scene import (
    DEFAULT_SEED,
    SceneBundle,
    SceneValidationError,
    generate_scene,
    load_scene,
    save_scene,
    sha256_bytes,
    validate_scene,
)
from .verify import compare_baseline


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _npy_bytes(array: np.ndarray) -> bytes:
    import io

    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array), allow_pickle=False)
    return buffer.getvalue()


def _validation_failure(out_dir: Path, warnings: list[dict[str, Any]]) -> int:
    report = {
        "status": "aborted",
        "business_codes": sorted({warning["code"] for warning in warnings}),
        "warnings": warnings,
        "image_written": False,
    }
    out_dir.mkdir(parents=True, exist_ok=True)
    write_json_atomic(out_dir / "validation_report.json", report)
    sys.stderr.write(json.dumps(report, separators=(",", ":"), sort_keys=True) + "\n")
    return 2


def command_generate(args: argparse.Namespace) -> int:
    scene = generate_scene(args.seed)
    bundle = validate_scene(scene)
    save_scene(scene, args.out)
    print(
        json.dumps(
            {
                "scene": str(Path(args.out).resolve()),
                "scene_digest": bundle.scene_digest,
                "triangle_count": int(bundle.triangles.shape[0]),
                "face_count": int(bundle.face_ids.shape[0]),
            },
            sort_keys=True,
        )
    )
    return 0


def command_tables(args: argparse.Namespace) -> int:
    try:
        bundle = validate_scene(load_scene(args.scene))
    except SceneValidationError as exc:
        return _validation_failure(Path(args.out), exc.warnings)
    tables = build_film_tables(bundle)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    thickness = tables.thickness_delta
    normals = tables.micro_normals
    thickness_path = out / "thickness_delta_nm.npy"
    normals_path = out / "micro_normals.npy"
    write_npy_atomic(thickness_path, thickness)
    write_npy_atomic(normals_path, normals)
    manifest = {
        "schema": "elytron-table-manifest/v1",
        "scene_digest": bundle.scene_digest,
        "face_count": int(bundle.face_ids.shape[0]),
        "table_size": tables.table_size,
        "thickness_delta_npy_sha256": sha256_bytes(thickness_path.read_bytes()),
        "micro_normals_npy_sha256": sha256_bytes(normals_path.read_bytes()),
        "thickness_delta_npy_header_sha256": sha256_bytes(_npy_bytes(thickness)),
        "micro_normals_npy_header_sha256": sha256_bytes(_npy_bytes(normals)),
    }
    write_json_atomic(out / "manifest.json", manifest)
    print(json.dumps(manifest, sort_keys=True))
    return 0


def _assemble(
    scene: dict[str, Any],
    specs: list[Any],
    tiles: dict[str, Any],
) -> dict[str, dict[str, np.ndarray]]:
    images: dict[str, dict[str, np.ndarray]] = {}
    for camera in scene["cameras"]:
        width = int(camera["width"])
        height = int(camera["height"])
        images[str(camera["id"])] = {
            "rgba": np.zeros((height, width, 4), dtype=np.uint8),
            "face_id": np.zeros((height, width), dtype=np.int32),
        }
    for spec in specs:
        tile = tiles.get(spec.block_id)
        if tile is None:
            raise RuntimeError(f"missing completed tile {spec.block_id}")
        target = images[spec.camera_id]
        target["rgba"][spec.y : spec.y + spec.height, spec.x : spec.x + spec.width] = tile.rgba
        target["face_id"][spec.y : spec.y + spec.height, spec.x : spec.x + spec.width] = tile.face_id
    return images


def _write_images(
    scene: dict[str, Any],
    images: dict[str, dict[str, np.ndarray]],
    out_dir: Path,
) -> dict[str, dict[str, str]]:
    hashes: dict[str, dict[str, str]] = {}
    for camera_index, camera in enumerate(scene["cameras"]):
        camera_id = str(camera["id"])
        rgba = images[camera_id]["rgba"]
        face = images[camera_id]["face_id"]
        rgba_npy = out_dir / f"C{camera_index:02d}.rgba.npy"
        face_npy = out_dir / f"C{camera_index:02d}.face_id.npy"
        rgba_png = out_dir / f"C{camera_index:02d}.rgba.png"
        face_png = out_dir / f"C{camera_index:02d}.face_id.png"
        write_npy_atomic(rgba_npy, rgba)
        write_npy_atomic(face_npy, face)
        rgba_png_bytes = png_rgba_bytes(rgba)
        face_png_bytes = png_gray16_bytes(np.clip(face, 0, 65535).astype(np.uint16))
        write_png_atomic(rgba_png, rgba_png_bytes)
        write_png_atomic(face_png, face_png_bytes)
        hashes[camera_id] = {
            "rgba_npy": _file_sha256(rgba_npy),
            "face_npy": _file_sha256(face_npy),
            "rgba_png": sha256_bytes(rgba_png_bytes),
            "face_png": sha256_bytes(face_png_bytes),
        }
    return hashes


def command_render(args: argparse.Namespace) -> int:
    out_dir = Path(args.out)
    for stale_name in ("difference_report.json", "validation_report.json", "interrupt_report.json"):
        try:
            (out_dir / stale_name).unlink()
        except FileNotFoundError:
            pass
    try:
        bundle = validate_scene(load_scene(args.scene))
    except SceneValidationError as exc:
        return _validation_failure(out_dir, exc.warnings)

    started = time.perf_counter()
    scene = bundle.scene
    out_dir.mkdir(parents=True, exist_ok=True)
    specs = enumerate_blocks(scene)
    snapshot_path = out_dir / "progress_snapshot.json"
    tiles: dict[str, Any] = {}
    records: dict[str, dict[str, Any]] = {}
    interrupt = {"requested": False, "signal": None}

    def request_interrupt(signum: int, _frame: Any) -> None:
        interrupt["requested"] = True
        try:
            interrupt["signal"] = signal.Signals(signum).name
        except ValueError:
            interrupt["signal"] = str(signum)

    previous_handlers: dict[int, Any] = {}
    for signum in (signal.SIGINT, getattr(signal, "SIGTERM", signal.SIGINT)):
        try:
            previous_handlers[signum] = signal.signal(signum, request_interrupt)
        except (ValueError, OSError):
            pass

    def restore_handlers() -> None:
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)

    def write_interrupt_report() -> None:
        completed = [spec.block_id for spec in specs[: len(records)]]
        next_id = (
            specs[len(records)].block_id if len(records) < len(specs) else None
        )
        report = {
            "status": "interrupted",
            "signal": interrupt["signal"],
            "completed_block_count": len(records),
            "completed_block_ids": completed,
            "next_block_id": next_id,
            "snapshot_committed": bool(records) and snapshot_path.exists(),
            "resume_command": f'--scene "{Path(args.scene)}" --out "{out_dir}" --resume',
            "exit_code": 0,
        }
        write_json_atomic(out_dir / "interrupt_report.json", report)

    resume_report: dict[str, Any] = {
        "snapshot_found": False,
        "action": "fresh",
        "reasons": [],
        "restored_blocks": 0,
        "next_block_index": 0,
    }
    if args.resume:
        resumed = load_snapshot(snapshot_path, bundle, specs)
        if resumed.granted:
            tiles = resumed.tiles
            records = resumed.records
            resume_report = {
                "snapshot_found": True,
                "action": "resumed",
                "reasons": [],
                "restored_blocks": len(records),
                "next_block_index": len(records),
            }
        else:
            remove_snapshot(snapshot_path)
            resume_report = {
                "snapshot_found": resumed.snapshot_found,
                "action": "discarded" if resumed.snapshot_found else "start_zero",
                "reasons": resumed.reasons,
                "restored_blocks": 0,
                "next_block_index": 0,
            }
    else:
        remove_snapshot(snapshot_path)
    write_json_atomic(out_dir / "resume_report.json", resume_report)

    if interrupt["requested"]:
        write_interrupt_report()
        restore_handlers()
        return 0

    context = build_context(bundle)
    if interrupt["requested"]:
        write_interrupt_report()
        restore_handlers()
        return 0

    start_index = len(records)
    for spec in specs[start_index:]:
        if interrupt["requested"]:
            break
        tile = render_block(context, scene["cameras"][spec.camera_index], spec)
        record = make_block_record(spec, tile)
        tiles[spec.block_id] = tile
        records[spec.block_id] = record
        save_snapshot(snapshot_path, bundle, specs, tiles, records)
        if args.block_delay_ms > 0:
            time.sleep(args.block_delay_ms / 1000.0)
        if interrupt["requested"]:
            break

    if interrupt["requested"]:
        write_interrupt_report()
        restore_handlers()
        return 0

    if len(records) != len(specs):
        raise RuntimeError("render ended before all canonical blocks were committed")
    images = _assemble(scene, specs, tiles)
    file_hashes = _write_images(scene, images, out_dir)
    hue = hue_continuity(context)
    report = build_report(
        bundle, context, specs, tiles, records, images, file_hashes, hue
    )
    write_json_atomic(out_dir / "report.json", report)

    exit_code = 0 if report["pass"] else 4
    if args.baseline:
        differences = compare_baseline(
            out_dir,
            args.baseline,
            scene,
            images,
            report,
        )
        write_json_atomic(out_dir / "difference_report.json", differences)
        if differences["status"] != "identical":
            exit_code = 3

    restore_handlers()
    elapsed = time.perf_counter() - started
    print(
        json.dumps(
            {
                "status": "complete" if exit_code == 0 else "failed",
                "exit_code": exit_code,
                "out": str(out_dir.resolve()),
                "blocks": len(records),
                "scene_digest": bundle.scene_digest,
                "elapsed_seconds": round(elapsed, 6),
                "report_pass": bool(report["pass"]),
                "resume_action": resume_report["action"],
            },
            sort_keys=True,
        )
    )
    return exit_code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="elytron-render",
        description="Deterministic thin-film elytron offscreen reference renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate the deterministic scene JSON")
    generate.add_argument("--seed", type=int, default=DEFAULT_SEED)
    generate.add_argument("--out", required=True)
    generate.set_defaults(func=command_generate)

    tables = subparsers.add_parser("dump-tables", help="write thickness and micro-normal tables")
    tables.add_argument("--scene", required=True)
    tables.add_argument("--out", required=True)
    tables.set_defaults(func=command_tables)

    render = subparsers.add_parser("render", help="validate, render, checkpoint, and report")
    render.add_argument("--scene", required=True)
    render.add_argument("--out", required=True)
    render.add_argument("--resume", action="store_true")
    render.add_argument("--baseline")
    render.add_argument(
        "--block-delay-ms",
        type=float,
        default=0.0,
        help="test-only pause after each committed block",
    )
    render.set_defaults(func=command_render)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if getattr(args, "block_delay_ms", 0.0) < 0:
        parser.error("--block-delay-ms must be non-negative")
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
