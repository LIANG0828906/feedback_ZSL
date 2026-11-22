from __future__ import annotations

import hashlib
import io
from pathlib import Path
from typing import Any

import numpy as np

from .model import PROJECT_FORMAT
from .util import atomic_write_bytes, atomic_write_json, ensure_dir

SCATTER_FORMAT = "terrain-scatter/1"
TRUTH_FORMAT = "terrain-truth/1"
DEFAULT_MATERIALS = (
    {"name": "grass", "default_param": 1.0},
    {"name": "rock", "default_param": 1.0},
    {"name": "dirt", "default_param": 1.0},
    {"name": "snow", "default_param": 1.0},
)


def generate_project(
    output_dir: str | Path,
    *,
    seed: int = 20261123,
    grid_tiles: tuple[int, int] = (4, 3),
    tile_size: int = 16,
    subdivisions: int = 4,
    points_per_tile: int = 96,
) -> dict[str, Any]:
    """Generate a deterministic local terrain project.

    Height and material lattices are generated in world space and sliced into
    tiles, so shared seam samples start bit-identical. Scatter points are
    generated from a per-tile seed derived from the project seed and tile id.
    """

    if tile_size <= 0:
        raise ValueError("tile_size must be positive")
    gx_count, gy_count = grid_tiles
    if gx_count <= 0 or gy_count <= 0:
        raise ValueError("grid_tiles must be positive")
    root = Path(output_dir).resolve()
    ensure_dir(root)
    tiles_dir = ensure_dir(root / "tiles")

    width = gx_count * tile_size + 1
    height_pixels = gy_count * tile_size + 1
    height_world = _height_field(width, height_pixels, seed)
    weights_world = _weights_field(height_world, seed)

    material_names = tuple(item["name"] for item in DEFAULT_MATERIALS)
    tile_entries: list[dict[str, Any]] = []
    for gy in range(gy_count):
        for gx in range(gx_count):
            tile_id = f"T{gy * gx_count + gx:04d}"
            tile_dir = ensure_dir(tiles_dir / tile_id)
            y0 = gy * tile_size
            x0 = gx * tile_size
            height = height_world[y0 : y0 + tile_size + 1, x0 : x0 + tile_size + 1].copy()
            weights = weights_world[y0 : y0 + tile_size + 1, x0 : x0 + tile_size + 1, :].copy()
            points = _scatter_points(tile_id, seed, tile_size, weights, points_per_tile)
            accepted = [p for p in points if _scatter_survives(p, weights, tile_size, 0.12)]
            truth = {
                "format": TRUTH_FORMAT,
                "height_samples": [
                    {"x": x, "y": y, "height": float(height[y, x])}
                    for x, y in _truth_height_points(tile_size)
                ],
                "material_coverage": {
                    name: float(weights[:, :, layer].astype(np.float64).mean() / 255.0)
                    for layer, name in enumerate(material_names)
                },
                "scatter_count": len(accepted),
                "scatter_count_by_kind": {
                    kind: sum(1 for point in accepted if point["kind"] == kind)
                    for kind in ("tree", "bush", "rock", "prop")
                },
            }

            height_rel = f"tiles/{tile_id}/height.npy"
            weights_rel = f"tiles/{tile_id}/weights.npy"
            scatter_rel = f"tiles/{tile_id}/scatter.json"
            truth_rel = f"tiles/{tile_id}/truth.json"
            _write_npy(tile_dir / "height.npy", height)
            _write_npy(tile_dir / "weights.npy", weights.astype(np.uint8, copy=False))
            atomic_write_json(
                tile_dir / "scatter.json",
                {
                    "format": SCATTER_FORMAT,
                    "seed": seed,
                    "points": points,
                },
            )
            atomic_write_json(tile_dir / "truth.json", truth)
            tile_entries.append(
                {
                    "id": tile_id,
                    "grid": [gx, gy],
                    "height_file": height_rel,
                    "weights_file": weights_rel,
                    "scatter_file": scatter_rel,
                    "truth_file": truth_rel,
                    "height_shape": [tile_size + 1, tile_size + 1],
                    "weights_shape": [
                        tile_size + 1,
                        tile_size + 1,
                        len(material_names),
                    ],
                }
            )

    manifest = {
        "format": PROJECT_FORMAT,
        "seed": seed,
        "tile_size": tile_size,
        "grid_tiles": [gx_count, gy_count],
        "subdivisions": subdivisions,
        "materials": list(DEFAULT_MATERIALS),
        "tiles": tile_entries,
    }
    atomic_write_json(root / "manifest.json", manifest)
    return manifest


def _height_field(width: int, height: int, seed: int) -> np.ndarray:
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float64)
    phase = (seed % 10_000) * 0.001
    value = (
        7.0 * np.sin(xx * 0.21 + phase)
        + 5.0 * np.cos(yy * 0.17 - phase * 1.7)
        + 2.4 * np.sin((xx + yy) * 0.12 + phase * 0.5)
        + 1.8 * np.cos(np.sqrt((xx - width * 0.62) ** 2 + (yy - height * 0.48) ** 2) * 0.3)
    )
    noise = np.sin(xx * 12.9898 + yy * 78.233 + seed * 0.001) * 43758.5453
    noise = (noise - np.floor(noise) - 0.5) * 1.6
    return np.ascontiguousarray(value + noise, dtype=np.float64)


def _weights_field(height: np.ndarray, seed: int) -> np.ndarray:
    yy, xx = np.mgrid[0 : height.shape[0], 0 : height.shape[1]].astype(np.float64)
    phase = (seed % 997) * 0.013
    snow = 0.22 + np.exp((height - float(np.percentile(height, 76))) / 2.8)
    rock = 0.42 + 0.58 * (0.5 + 0.5 * np.sin(xx * 0.31 + yy * 0.19 + phase))
    grass = 0.38 + 0.62 * (0.5 + 0.5 * np.cos(xx * 0.23 - yy * 0.27 + phase * 1.9))
    dirt = 0.28 + 0.72 * (0.5 + 0.5 * np.sin((xx - yy) * 0.16 + phase * 0.7))
    scores = np.stack([grass, rock, dirt, snow], axis=-1)
    scores = np.clip(scores, 0.01, None)
    normalized = scores / scores.sum(axis=-1, keepdims=True) * 255.0
    base = np.floor(normalized).astype(np.int64)
    remainder = normalized - base
    deficit = 255 - base.sum(axis=-1)
    order = np.argsort(-remainder, axis=-1, kind="stable")
    rows, cols = np.mgrid[0 : height.shape[0], 0 : height.shape[1]]
    for rank in range(int(deficit.max()) if deficit.size else 0):
        mask = deficit > rank
        selected = order[:, :, min(rank, order.shape[2] - 1)]
        base[rows[mask], cols[mask], selected[mask]] += 1
    return np.ascontiguousarray(np.clip(base, 0, 255), dtype=np.uint8)


def _scatter_points(
    tile_id: str,
    seed: int,
    tile_size: int,
    weights: np.ndarray,
    count: int,
) -> list[dict[str, Any]]:
    tile_digest = hashlib.sha256(tile_id.encode("utf-8")).hexdigest()
    local_seed = int(seed) ^ int(tile_digest[:16], 16)
    rng = np.random.default_rng(local_seed)
    rix = rng.random(count)
    riy = rng.random(count)
    rscale = rng.random(count)
    rrot = rng.random(count)
    rvar = rng.random(count)
    rkind = rng.random(count)
    points: list[dict[str, Any]] = []
    for index in range(count):
        x = float(np.round(rix[index] * (tile_size - 1e-4), 6))
        y = float(np.round(riy[index] * (tile_size - 1e-4), 6))
        sampled = _sample_weights(weights, x, y)
        layer_index = int(np.argmax(sampled))
        layer = DEFAULT_MATERIALS[layer_index]["name"]
        roll = float(rkind[index])
        if layer == "grass":
            kind = "tree" if roll < 0.68 else "bush"
        elif layer == "rock":
            kind = "rock" if roll < 0.82 else "prop"
        elif layer == "dirt":
            kind = "bush" if roll < 0.62 else "prop"
        else:
            kind = "tree" if roll < 0.74 else "rock"
        points.append(
            {
                "id": f"{tile_id}-P{index:04d}",
                "kind": kind,
                "layer": layer,
                "x": x,
                "y": y,
                "scale": float(np.round(0.65 + 0.9 * rscale[index], 6)),
                "rotation": float(np.round(rrot[index] * 2.0 * np.pi, 6)),
                "variant": float(np.round(rvar[index], 6)),
            }
        )
    points.sort(key=lambda item: item["id"])
    return points


def _sample_weights(weights: np.ndarray, x: float, y: float) -> np.ndarray:
    max_coord = weights.shape[0] - 1
    x = min(max(float(x), 0.0), float(max_coord))
    y = min(max(float(y), 0.0), float(max_coord))
    x0 = min(int(np.floor(x)), max_coord - 1)
    y0 = min(int(np.floor(y)), max_coord - 1)
    fx = x - x0
    fy = y - y0
    top = (
        weights[y0, x0].astype(np.float64) * (1.0 - fx)
        + weights[y0, x0 + 1].astype(np.float64) * fx
    )
    bottom = (
        weights[y0 + 1, x0].astype(np.float64) * (1.0 - fx)
        + weights[y0 + 1, x0 + 1].astype(np.float64) * fx
    )
    return (top * (1.0 - fy) + bottom * fy) / 255.0


def _scatter_survives(
    point: dict[str, Any],
    weights: np.ndarray,
    tile_size: int,
    minimum_weight: float,
) -> bool:
    if not (0.0 <= point["x"] < tile_size and 0.0 <= point["y"] < tile_size):
        return False
    layer_index = next(
        index
        for index, material in enumerate(DEFAULT_MATERIALS)
        if material["name"] == point["layer"]
    )
    sampled = _sample_weights(weights, point["x"], point["y"])
    return bool(sampled[layer_index] >= minimum_weight)


def _truth_height_points(tile_size: int) -> list[tuple[int, int]]:
    return sorted(
        {
            (0, 0),
            (tile_size, 0),
            (0, tile_size),
            (tile_size, tile_size),
            (tile_size // 2, tile_size // 3),
            (tile_size // 3, tile_size // 2),
            (tile_size // 2, tile_size // 2),
        }
    )


def _write_npy(path: Path, array: np.ndarray) -> None:
    buffer = io.BytesIO()
    np.save(buffer, array, allow_pickle=False)
    atomic_write_bytes(path, buffer.getvalue())
