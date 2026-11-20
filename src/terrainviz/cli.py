from __future__ import annotations

import argparse
import hashlib
import sys
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .camera import Camera
from .demo import run_demo, _save_npy
from .errors import TerrainError
from .heightfield import HeightField
from .io_utils import atomic_write_json, canonical_json_bytes
from .material import MaterialMap
from .render import RenderConfig
from .samples import generate_sample_set, write_baseline_trajectory
from .service import RenderService
from .snapshot import load_snapshot, recover_terrain_from_disk, snapshot_changes
from .terrain import Terrain


def _default_camera(terrain: Terrain, width: int, height: int) -> Camera:
    world_width = terrain.heightfield.world_width
    world_depth = terrain.heightfield.world_height
    mean_height = float(np.mean(terrain.heightfield.heights))
    return Camera(
        eye=(
            world_width * 0.08,
            mean_height + max(world_width, world_depth) * 0.72,
            world_depth * 1.02,
        ),
        target=(world_width * 0.52, mean_height * 0.85, world_depth * 0.46),
        up=(0.0, 1.0, 0.0),
        fov_y_degrees=48.0,
        width=width,
        height=height,
    )


def _print_json(value: Any) -> None:
    sys.stdout.buffer.write(canonical_json_bytes(value) + b"\n")
    sys.stdout.buffer.flush()


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="terrainviz",
        description="Deterministic NumPy terrain LOD and offscreen renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate local terrain samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=20261120)
    generate.add_argument("--rows", type=int, default=65)
    generate.add_argument("--cols", type=int, default=81)
    generate.add_argument("--block-size", type=int, default=16)
    generate.add_argument("--threshold", type=float, default=1.5)
    generate.add_argument("--seam-tolerance", type=float, default=1.0)
    generate.add_argument("--no-baseline", action="store_true")

    render = subparsers.add_parser("render", help="render a heightfield offscreen")
    render.add_argument("--terrain", required=True)
    render.add_argument("--material")
    render.add_argument("--out", required=True)
    render.add_argument("--snapshot")
    render.add_argument("--block-size", type=int, default=16)
    render.add_argument("--max-level", type=int, default=4)
    render.add_argument("--threshold", type=float, default=1.5)
    render.add_argument("--seam-tolerance", type=float, default=1.0)
    render.add_argument("--precision", choices=("float32", "float64"), default="float64")
    render.add_argument("--width", type=int, default=96)
    render.add_argument("--height", type=int, default=72)
    render.add_argument("--fov", type=float, default=48.0)
    render.add_argument("--eye", nargs=3, type=float)
    render.add_argument("--target", nargs=3, type=float)
    render.add_argument("--up", nargs=3, type=float, default=(0.0, 1.0, 0.0))

    demo = subparsers.add_parser("demo", help="generate samples and run the full demo")
    demo.add_argument("--out", required=True)
    demo.add_argument("--seed", type=int, default=20261120)
    demo.add_argument("--threshold", type=float, default=1.5)
    demo.add_argument("--seam-tolerance", type=float, default=1.0)
    demo.add_argument("--block-size", type=int, default=16)

    inspect = subparsers.add_parser("snapshot", help="validate and inspect a snapshot")
    inspect.add_argument("--snapshot", required=True)

    recover = subparsers.add_parser(
        "recover", help="rebuild a terrain from a committed snapshot and source files"
    )
    recover.add_argument("--terrain", required=True)
    recover.add_argument("--snapshot", required=True)
    recover.add_argument("--material")
    return parser


def _command_generate(args: argparse.Namespace) -> dict[str, Any]:
    samples = generate_sample_set(args.out, seed=args.seed, rows=args.rows, cols=args.cols)
    result: dict[str, Any] = {
        "format": "terrainviz-generate-result",
        "seed": args.seed,
        "samples": [
            {
                "name": sample.name,
                "heightfield": sample.heightfield_path.name,
                "material": sample.material_path.name,
                "heightfield_sha256": sample.height_digest,
                "material_sha256": sample.material_digest,
            }
            for sample in samples
        ],
    }
    if not args.no_baseline:
        trajectory = write_baseline_trajectory(
            samples,
            Path(args.out) / "baseline_trajectory.jsonl",
            screen_error_threshold=args.threshold,
            seam_tolerance=args.seam_tolerance,
            block_size=args.block_size,
        )
        result["baseline_trajectory"] = trajectory.name
    return result


def _command_render(args: argparse.Namespace) -> dict[str, Any]:
    heightfield = HeightField.load(args.terrain)
    material = MaterialMap.load(args.material) if args.material else None
    terrain = Terrain(heightfield, material, block_size=args.block_size)
    if args.eye is None or args.target is None:
        default_camera = _default_camera(terrain, args.width, args.height)
        eye = args.eye if args.eye is not None else default_camera.eye
        target = (
            args.target if args.target is not None else default_camera.target
        )
    else:
        eye = args.eye
        target = args.target
    camera = Camera(
        eye=tuple(eye),
        target=tuple(target),
        up=tuple(args.up),
        fov_y_degrees=args.fov,
        width=args.width,
        height=args.height,
    )
    previous_snapshot = None
    if args.snapshot and Path(args.snapshot).exists():
        previous_snapshot = load_snapshot(args.snapshot)
    service = RenderService(terrain)
    result = service.render(
        camera,
        args.threshold,
        args.seam_tolerance,
        max_level=args.max_level,
        config=RenderConfig(precision=args.precision),
        snapshot_path=args.snapshot,
        heightfield_name=Path(args.terrain).name,
        material_name=Path(args.material).name if args.material else None,
    )
    output = Path(args.out)
    color_file = _save_npy(output / "color.npy", result.color)
    depth_file = _save_npy(output / "depth.npy", result.depth)
    heat_file = _save_npy(output / "error_heatmap.npy", result.error_heatmap)
    changes = None
    if previous_snapshot is not None:
        changes = snapshot_changes(
            previous_snapshot,
            camera,
            {item["block_id"]: item["level"] for item in result.report["levels"]},
            terrain.material.material_version if terrain.material else None,
            args.threshold,
        )
    report = dict(result.report)
    report["outputs"] = {
        "color.npy": color_file,
        "depth.npy": depth_file,
        "error_heatmap.npy": heat_file,
        "snapshot": service.last_snapshot.digest if service.last_snapshot else None,
    }
    report["previous_snapshot_diff"] = changes
    report["report_hash"] = hashlib.sha256(canonical_json_bytes(report)).hexdigest()
    atomic_write_json(output / "render_report.json", report)
    return {
        "format": "terrainviz-render-result",
        "buffers": result.report["buffers"],
        "combined_buffer_sha256": result.combined_digest,
        "block_levels": [
            {"block_id": item["block_id"], "level": item["level"]}
            for item in result.report["levels"]
        ],
        "error_summary": result.report["error_summary"],
        "seam_summary": result.seam_result.summary(),
        "precision_switch_diff": result.report["pixel_diff"],
        "previous_snapshot_diff": changes,
        "outputs": report["outputs"],
        "report": "render_report.json",
        "report_hash": report["report_hash"],
    }


def _command_demo(args: argparse.Namespace) -> dict[str, Any]:
    report = run_demo(
        args.out,
        seed=args.seed,
        screen_error_threshold=args.threshold,
        seam_tolerance=args.seam_tolerance,
        block_size=args.block_size,
    )
    return {
        "format": "terrainviz-demo-result",
        "sample_count": report["sample_count"],
        "all_repeat_renders_identical": report["all_repeat_renders_identical"],
        "report_hash": report["report_hash"],
        "report": "report.json",
        "terrains": [
            {
                "terrain": item["terrain"],
                "combined_buffer_sha256": item["combined_buffer_sha256"],
                "snapshot_sha256": item["snapshot_sha256"],
                "seam_status": item["seam_summary"]["status"],
                "repeat_render_identical": item["repeat_render_identical"],
            }
            for item in report["terrain_reports"]
        ],
    }


def _command_snapshot(args: argparse.Namespace) -> dict[str, Any]:
    snapshot = load_snapshot(args.snapshot)
    return {
        "valid": True,
        "digest": snapshot.digest,
        "version": snapshot.content["version"],
        "format": snapshot.content["format"],
        "heightfield": snapshot.content["heightfield"],
        "block_count": len(snapshot.content["block_levels"]),
        "material_version": snapshot.content["material_version"],
    }


def _command_recover(args: argparse.Namespace) -> dict[str, Any]:
    terrain, snapshot = recover_terrain_from_disk(
        args.terrain, args.snapshot, args.material
    )
    return {
        "format": "terrainviz-recovery-result",
        "snapshot_digest": snapshot.digest,
        "heightfield_digest": terrain.heightfield.height_digest,
        "block_size": terrain.block_size,
        "spacing": terrain.spacing,
        "block_count": len(terrain.blocks),
        "node_count": terrain.node_count,
        "committed_levels": snapshot.content["block_levels"],
        "note": "recovered from disk snapshot metadata and source files only",
    }


def main(argv: Sequence[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            result = _command_generate(args)
        elif args.command == "render":
            result = _command_render(args)
        elif args.command == "demo":
            result = _command_demo(args)
        elif args.command == "snapshot":
            result = _command_snapshot(args)
        elif args.command == "recover":
            result = _command_recover(args)
        else:
            parser.error(f"unknown command: {args.command}")
            return 2
        _print_json(result)
        return 0
    except TerrainError as exc:
        sys.stderr.write(canonical_json_bytes(exc.to_dict()).decode("utf-8") + "\n")
        return 2
    except (OSError, ValueError) as exc:
        sys.stderr.write(
            canonical_json_bytes(
                {
                    "code": "E_INVALID_ARGUMENT",
                    "message": str(exc),
                    "details": {},
                }
            ).decode("utf-8")
            + "\n"
        )
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

