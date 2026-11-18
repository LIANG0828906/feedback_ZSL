"""Block rendering, image-space output conversion and final assembly."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .patterns import PatternSet
from .png import encode_png_rgba
from .projection import ProjectionSystem
from .scene import Scene
from .shading import camera_rays, shade_pixels
from .tiles import read_tile
from .util import (
    atomic_write_bytes,
    atomic_write_npy,
    sha256_bytes,
    srgb_encode,
)


@dataclass
class BlockResult:
    angle_index: int
    block_index: int
    x0: int
    y0: int
    rgba: np.ndarray
    owner_id: np.ndarray
    owner_layer: np.ndarray
    stats: dict[str, Any]

    @property
    def width(self) -> int:
        return int(self.rgba.shape[1])

    @property
    def height(self) -> int:
        return int(self.rgba.shape[0])


def color_summary(
    rgba: np.ndarray, owner_id: np.ndarray, owner_layer: np.ndarray
) -> dict[str, Any]:
    rgba_contiguous = np.ascontiguousarray(rgba)
    owner_id_contiguous = np.ascontiguousarray(owner_id)
    owner_layer_contiguous = np.ascontiguousarray(owner_layer)
    layer_counts = {
        str(layer): int(np.count_nonzero(owner_layer == layer)) for layer in range(4)
    }
    layer_counts["none"] = int(np.count_nonzero(owner_layer == 255))
    return {
        "rgba_mean": [float(value) for value in np.mean(rgba_contiguous, axis=(0, 1))],
        "rgba_min": [float(value) for value in np.min(rgba_contiguous, axis=(0, 1))],
        "rgba_max": [float(value) for value in np.max(rgba_contiguous, axis=(0, 1))],
        "rgba_sha256": sha256_bytes(rgba_contiguous.tobytes(order="C")),
        "owner_id_sha256": sha256_bytes(owner_id_contiguous.tobytes(order="C")),
        "owner_layer_sha256": sha256_bytes(
            owner_layer_contiguous.tobytes(order="C")
        ),
        "owner_layer_counts": layer_counts,
        "owner_leaf_pixels": int(np.count_nonzero(owner_id > 0)),
    }


def render_block(
    scene: Scene,
    patterns: PatternSet,
    projection: ProjectionSystem,
    angle_index: int,
    block_index: int,
) -> BlockResult:
    blocks_x, blocks_y = scene.render.block_counts(
        scene.camera.width, scene.camera.height
    )
    blocks_per_angle = blocks_x * blocks_y
    if angle_index < 0 or angle_index >= len(scene.camera.angles_deg):
        raise IndexError("angle index outside scene camera samples")
    if block_index < 0 or block_index >= blocks_per_angle:
        raise IndexError("block index outside angle tile grid")
    block_y, block_x = divmod(block_index, blocks_x)
    x0 = block_x * scene.render.tile_width
    y0 = block_y * scene.render.tile_height
    width = min(scene.render.tile_width, scene.camera.width - x0)
    height = min(scene.render.tile_height, scene.camera.height - y0)
    xs = x0 + np.arange(width, dtype=np.float64)
    ys = y0 + np.arange(height, dtype=np.float64)
    yy, xx = np.meshgrid(ys, xs, indexing="ij")
    origins, directions = camera_rays(
        scene, angle_index, xx.reshape(-1), yy.reshape(-1)
    )
    shaded = shade_pixels(scene, patterns, projection, angle_index, origins, directions)
    rgba = shaded.rgba.reshape(height, width, 4)
    owner_id = shaded.owner_id.reshape(height, width)
    owner_layer = shaded.owner_layer.reshape(height, width)
    summary = color_summary(rgba, owner_id, owner_layer)
    stats = {
        "angle_index": angle_index,
        "block_index": block_index,
        "pixel_origin": [x0, y0],
        "pixel_size": [width, height],
        "color_summary": summary,
        "reflection_sums": [float(value) for value in shaded.reflection_sums],
        "transmission_sums": [float(value) for value in shaded.transmission_sums],
        "hit_counts": [int(value) for value in shaded.hit_counts],
        "max_specular": float(shaded.max_specular),
        "clipped_before_tonemap": int(shaded.clipped_before_tonemap),
        "receiver_pixels": int(shaded.receiver_pixels),
    }
    return BlockResult(
        angle_index=angle_index,
        block_index=block_index,
        x0=x0,
        y0=y0,
        rgba=np.ascontiguousarray(rgba.astype(np.float32)),
        owner_id=np.ascontiguousarray(owner_id.astype(np.int32)),
        owner_layer=np.ascontiguousarray(owner_layer.astype(np.uint8)),
        stats=stats,
    )


def _tone_map(linear_color: np.ndarray, white: float) -> np.ndarray:
    safe_white = max(float(white), 1e-6)
    return np.maximum(linear_color, 0.0) / (
        1.0 + np.maximum(linear_color, 0.0) / safe_white
    )


def display_rgba8(scene: Scene, rgba: np.ndarray) -> np.ndarray:
    alpha = np.clip(rgba[..., 3], 0.0, 1.0)
    color = rgba[..., 0:3] / np.maximum(alpha[..., None], 1e-8)
    mapped = _tone_map(color, scene.render.tone_map_white)
    srgb = srgb_encode(mapped)
    premultiplied = np.clip(srgb * alpha[..., None], 0.0, 1.0)
    result = np.empty((*rgba.shape[:2], 4), dtype=np.uint8)
    result[..., 0:3] = np.floor(premultiplied * 255.0 + 0.5).astype(np.uint8)
    result[..., 3] = np.floor(alpha * 255.0 + 0.5).astype(np.uint8)
    return result


def attribution_rgba8(owner_id: np.ndarray, owner_layer: np.ndarray) -> np.ndarray:
    palette = np.asarray(
        [
            [0.18, 0.42, 0.82],
            [0.10, 0.68, 0.52],
            [0.95, 0.64, 0.16],
            [0.90, 0.24, 0.31],
        ],
        dtype=np.float64,
    )
    result = np.zeros((*owner_id.shape, 4), dtype=np.uint8)
    for layer in range(4):
        mask = owner_layer == layer
        if not np.any(mask):
            continue
        leaf_ids = (owner_id[mask] - 1) % 10000
        variation = 0.63 + 0.37 * ((leaf_ids % 16) / 15.0)
        colors = palette[layer][None, :] * variation[:, None]
        result[mask, 0:3] = np.floor(colors * 255.0 + 0.5).astype(np.uint8)
        result[mask, 3] = 255
    result[owner_id == -2, 0:3] = np.asarray([118, 112, 104], dtype=np.uint8)
    result[owner_id == -2, 3] = 255
    result[owner_id == -3, 0:3] = np.asarray([146, 138, 127], dtype=np.uint8)
    result[owner_id == -3, 3] = 255
    result[owner_id == -1, 0:3] = 0
    result[owner_id == -1, 3] = 0
    return result


def assemble_angle(
    scene: Scene,
    workdir: str | Path,
    angle_index: int,
    tile_entries: list[dict[str, Any]],
) -> dict[str, Any]:
    workdir = Path(workdir)
    blocks_x, blocks_y = scene.render.block_counts(
        scene.camera.width, scene.camera.height
    )
    blocks_per_angle = blocks_x * blocks_y
    angle_entries = sorted(
        [entry for entry in tile_entries if entry["angle_index"] == angle_index],
        key=lambda item: item["block_index"],
    )
    if len(angle_entries) != blocks_per_angle:
        raise ValueError(
            f"angle {angle_index} has {len(angle_entries)} blocks, expected {blocks_per_angle}"
        )
    rgba = np.zeros((scene.camera.height, scene.camera.width, 4), dtype=np.float32)
    owner_id = np.full((scene.camera.height, scene.camera.width), -1, dtype=np.int32)
    owner_layer = np.full(
        (scene.camera.height, scene.camera.width), 255, dtype=np.uint8
    )
    for entry in angle_entries:
        tile = read_tile(workdir / entry["tile_file"])
        x0, y0 = entry["pixel_origin"]
        height, width = tile.rgba.shape[:2]
        rgba[y0 : y0 + height, x0 : x0 + width, :] = tile.rgba
        owner_id[y0 : y0 + height, x0 : x0 + width] = tile.owner_id
        owner_layer[y0 : y0 + height, x0 : x0 + width] = tile.owner_layer
    renders_dir = workdir / "renders"
    renders_dir.mkdir(parents=True, exist_ok=True)
    prefix = f"angle_{angle_index:03d}"
    rgba_npy = renders_dir / f"{prefix}_rgba_premultiplied_linear.npy"
    owner_id_npy = renders_dir / f"{prefix}_owner_id.npy"
    owner_layer_npy = renders_dir / f"{prefix}_owner_layer.npy"
    rgba_png = renders_dir / f"{prefix}_rgba.png"
    owner_png = renders_dir / f"{prefix}_owner.png"
    atomic_write_npy(rgba_npy, rgba)
    atomic_write_npy(owner_id_npy, owner_id)
    atomic_write_npy(owner_layer_npy, owner_layer)
    png_data = encode_png_rgba(display_rgba8(scene, rgba))
    owner_png_data = encode_png_rgba(attribution_rgba8(owner_id, owner_layer))
    atomic_write_bytes(rgba_png, png_data)
    atomic_write_bytes(owner_png, owner_png_data)
    return {
        "angle_index": angle_index,
        "azimuth_deg": float(scene.camera.angles_deg[angle_index]),
        "files": {
            "rgba_premultiplied_linear_npy": str(rgba_npy.relative_to(workdir).as_posix()),
            "rgba_png": str(rgba_png.relative_to(workdir).as_posix()),
            "owner_id_npy": str(owner_id_npy.relative_to(workdir).as_posix()),
            "owner_layer_npy": str(owner_layer_npy.relative_to(workdir).as_posix()),
            "owner_png": str(owner_png.relative_to(workdir).as_posix()),
        },
        "sha256": {
            "rgba_premultiplied_linear_npy": sha256_bytes(
                np.ascontiguousarray(rgba).tobytes(order="C")
            ),
            "rgba_png": sha256_bytes(png_data),
            "owner_id_npy": sha256_bytes(
                np.ascontiguousarray(owner_id).tobytes(order="C")
            ),
            "owner_layer_npy": sha256_bytes(
                np.ascontiguousarray(owner_layer).tobytes(order="C")
            ),
            "owner_png": sha256_bytes(owner_png_data),
        },
        "rgba_mean": [
            float(value) for value in np.mean(rgba, axis=(0, 1))
        ],
        "owner_leaf_pixels": int(np.count_nonzero(owner_id > 0)),
    }


def landing_document(
    scene: Scene, patterns: PatternSet, records: list[dict[str, Any]]
) -> dict[str, Any]:
    from .util import object_digest

    return {
        "format": "GLV-LANDINGS-V1",
        "units": scene.raw.get("units", "metres"),
        "scene_digest": scene.digest,
        "pattern_digest": patterns.digest,
        "coordinate_system": "world metres; x right, y up, z toward camera",
        "camera_dependent": False,
        "out_of_receiver_policy": "contribution=0, aperture field outside facade=unoccluded (1)",
        "landing_count": len(records),
        "digest": object_digest(records),
        "points": records,
    }
