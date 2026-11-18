from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np

from .errors import OutputMismatchError
from .projection import ProjectionData
from .render import BlockResult, block_specs
from .report import build_stats
from .scene import Scene
from .util import (
    atomic_write_bytes,
    atomic_write_json,
    sha256_bytes,
    write_npy,
    write_png,
)


def assemble_blocks(scene: Scene, blocks: dict[str, BlockResult]) -> tuple[np.ndarray, np.ndarray, list[BlockResult]]:
    specs = block_specs(scene)
    missing = [spec.block_id for spec in specs if spec.block_id not in blocks]
    if missing:
        raise ValueError(f"cannot assemble incomplete render; missing {missing[:4]}")
    rgba = np.zeros((scene.height, scene.width, 4), dtype=np.uint8)
    layer_map = np.zeros((scene.height, scene.width), dtype=np.uint8)
    ordered: list[BlockResult] = []
    for spec in specs:
        result = blocks[spec.block_id]
        if result.rgba.shape != (spec.height, spec.width, 4):
            raise ValueError(f"RGBA shape mismatch for {spec.block_id}")
        if result.layer_map.shape != (spec.height, spec.width):
            raise ValueError(f"layer-map shape mismatch for {spec.block_id}")
        rgba[spec.y0 : spec.y1, spec.x0 : spec.x1] = result.rgba
        layer_map[spec.y0 : spec.y1, spec.x0 : spec.x1] = result.layer_map
        ordered.append(result)
    return rgba, layer_map, ordered


def write_render_outputs(
    scene: Scene,
    out_dir: str | Path,
    rgba: np.ndarray,
    layer_map: np.ndarray,
    ordered_blocks: list[BlockResult],
    projection: ProjectionData,
) -> dict[str, Any]:
    destination = Path(out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    rgba_bytes = np.ascontiguousarray(rgba, dtype=np.uint8).tobytes(order="C")
    layer_bytes = np.ascontiguousarray(layer_map, dtype=np.uint8).tobytes(order="C")
    atomic_write_bytes(destination / "rgba.bin", rgba_bytes)
    atomic_write_bytes(destination / "layer_map.bin", layer_bytes)
    write_npy(destination / "rgba.npy", rgba)
    write_npy(destination / "layer_map.npy", layer_map)
    write_png(destination / "rgba.png", rgba)
    write_png(destination / "layer_map.png", layer_map)

    projection_report = projection.report_dict(scene)
    atomic_write_json(destination / "projection_points.json", projection_report)
    stats = build_stats(scene, rgba, layer_map, ordered_blocks, projection)
    atomic_write_json(destination / "stats.json", stats)

    file_names = [
        "rgba.bin",
        "layer_map.bin",
        "rgba.npy",
        "layer_map.npy",
        "rgba.png",
        "layer_map.png",
        "projection_points.json",
        "stats.json",
    ]
    manifest = {
        "schema_version": 1,
        "scene_sha256": scene.digest,
        "width": scene.width,
        "height": scene.height,
        "supersampled_width": scene.width * scene.render.supersampling,
        "supersampled_height": scene.height * scene.render.supersampling,
        "block_width": scene.render.block_width,
        "block_height": scene.render.block_height,
        "block_count": len(ordered_blocks),
        "rgba_format": "uint8 HxWx4, C order, premultiplied alpha",
        "layer_map_format": "uint8 HxW, 0..N-1 leather layer, 255 background",
        "projection_samples": scene.render.projection_samples,
        "files": {
            name: {
                "sha256": sha256_bytes((destination / name).read_bytes()),
                "size": (destination / name).stat().st_size,
            }
            for name in file_names
        },
        "block_color_sha256": {
            block.spec.block_id: block.rgba_sha256 for block in ordered_blocks
        },
    }
    atomic_write_json(destination / "manifest.json", manifest)
    return stats


def _block_id_for_pixel(scene: Scene, x: int, y: int) -> str:
    row = y // scene.render.block_height
    col = x // scene.render.block_width
    return f"r{row:03d}_c{col:03d}"


def compare_outputs(
    scene: Scene,
    actual_dir: str | Path,
    baseline_dir: str | Path,
    *,
    max_differences: int = 512,
) -> dict[str, Any]:
    actual = Path(actual_dir)
    baseline = Path(baseline_dir)
    actual_rgba = np.fromfile(actual / "rgba.bin", dtype=np.uint8).reshape(
        scene.height,
        scene.width,
        4,
    )
    baseline_rgba = np.fromfile(baseline / "rgba.bin", dtype=np.uint8).reshape(
        scene.height,
        scene.width,
        4,
    )
    actual_layers = np.fromfile(actual / "layer_map.bin", dtype=np.uint8).reshape(
        scene.height,
        scene.width,
    )
    baseline_layers = np.fromfile(baseline / "layer_map.bin", dtype=np.uint8).reshape(
        scene.height,
        scene.width,
    )

    differences: list[dict[str, Any]] = []
    channel_names = ["R", "G", "B", "A"]
    rgba_mismatch = actual_rgba != baseline_rgba
    rgba_pixels = np.argwhere(np.any(rgba_mismatch, axis=2))
    rgba_channel_count = int(np.count_nonzero(rgba_mismatch))
    for y, x in rgba_pixels:
        for channel in range(4):
            if actual_rgba[y, x, channel] == baseline_rgba[y, x, channel]:
                continue
            if len(differences) >= max_differences:
                break
            differences.append(
                {
                    "block_id": _block_id_for_pixel(scene, int(x), int(y)),
                    "kind": "rgba",
                    "x": int(x),
                    "y": int(y),
                    "channel": channel_names[channel],
                    "actual": int(actual_rgba[y, x, channel]),
                    "baseline": int(baseline_rgba[y, x, channel]),
                }
            )
        if len(differences) >= max_differences:
            break

    layer_mismatch = actual_layers != baseline_layers
    layer_pixels = np.argwhere(layer_mismatch)
    layer_pixel_count = int(np.count_nonzero(layer_mismatch))
    for y, x in layer_pixels:
        if len(differences) >= max_differences:
            break
        differences.append(
            {
                "block_id": _block_id_for_pixel(scene, int(x), int(y)),
                "kind": "layer_map",
                "x": int(x),
                "y": int(y),
                "channel": "layer",
                "actual": int(actual_layers[y, x]),
                "baseline": int(baseline_layers[y, x]),
            }
        )

    actual_projection = (actual / "projection_points.json").read_bytes()
    baseline_projection = (baseline / "projection_points.json").read_bytes()
    projection_mismatch = actual_projection != baseline_projection
    if projection_mismatch and len(differences) < max_differences:
        differences.append(
            {
                "block_id": None,
                "kind": "projection_points",
                "x": None,
                "y": None,
                "channel": "json",
                "actual_sha256": sha256_bytes(actual_projection),
                "baseline_sha256": sha256_bytes(baseline_projection),
            }
        )

    result = {
        "ok": not (rgba_channel_count or layer_pixel_count or projection_mismatch),
        "code": "OUTPUT_MATCH" if not (rgba_channel_count or layer_pixel_count or projection_mismatch) else "OUTPUT_MISMATCH",
        "summary": {
            "rgba_differing_channels": rgba_channel_count,
            "layer_map_differing_pixels": layer_pixel_count,
            "projection_points_differ": projection_mismatch,
        },
        "differences": differences,
    }
    if not result["ok"]:
        atomic_write_json(actual / "diff.json", result)
        raise OutputMismatchError(differences)
    return result

