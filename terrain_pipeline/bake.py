from __future__ import annotations

import math
import struct
import threading
from collections.abc import Mapping
from typing import Any

import numpy as np

from .errors import E_BAKE, E_CANCEL, E_RESOURCE, E_TRUTH, PipelineError
from .model import BakedTile, BakeParams, TerrainTile
from .util import canonical_json_bytes, sha256_bytes

ARTIFACT_ORDER = (
    "positions.bin",
    "normals.bin",
    "indices.bin",
    "weights.bin",
    "instances.bin",
    "bounds.bin",
)
BLOCK_FORMAT = "terrain-render-block/1"
KIND_CODES = {"tree": 1, "bush": 2, "rock": 3, "prop": 4}
HEIGHT_TOLERANCE = 1.0e-4
COVERAGE_TOLERANCE = 0.02


class CancellationToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def check(self, stage: str) -> None:
        if self._event.is_set():
            raise PipelineError(
                E_CANCEL,
                "bake was cancelled",
                details={"stage": stage},
            )


def bake_tile(
    tile: TerrainTile,
    params: BakeParams,
    cache_key: str,
    dependencies: tuple[dict[str, Any], ...],
    neighbor_heights: Mapping[str, np.ndarray],
    *,
    verify_truth: bool = True,
    cancel_token: CancellationToken | None = None,
    operation_tag: str | None = None,
    fault_tiles: set[str] | None = None,
) -> BakedTile:
    """Bake one tile into deterministic, canonical render artifacts."""

    if cancel_token is not None:
        cancel_token.check(f"{tile.tile_id}:start")
    if fault_tiles and tile.tile_id in fault_tiles:
        raise PipelineError(
            E_BAKE,
            "injected tile bake failure",
            tile_id=tile.tile_id,
            field=None,
            details={"injected": True, "operation_tag": operation_tag or ""},
        )

    subdivisions = params.subdivisions
    mesh_side = tile.tile_size * subdivisions + 1
    vertex_count = mesh_side * mesh_side
    if vertex_count > params.max_vertices_per_thread:
        raise PipelineError(
            E_RESOURCE,
            "tile vertex count exceeds the per-thread vertex buffer limit",
            tile_id=tile.tile_id,
            details={
                "vertex_count": vertex_count,
                "max_vertices_per_thread": params.max_vertices_per_thread,
                "subdivisions": subdivisions,
            },
        )

    coords = np.arange(mesh_side, dtype=np.float64) / float(subdivisions)
    dense_height, _ = _resample_height(tile.height, coords)
    if cancel_token is not None:
        cancel_token.check(f"{tile.tile_id}:height")
    dzdx, dzdy = _normals(
        dense_height,
        tile,
        subdivisions,
        neighbor_heights,
        params.normal_strength,
    )
    normals = np.stack([-dzdx, -dzdy, np.ones_like(dzdx)], axis=-1)
    lengths = np.sqrt(np.sum(normals * normals, axis=-1, keepdims=True))
    normals = normals / lengths
    if cancel_token is not None:
        cancel_token.check(f"{tile.tile_id}:normals")

    origin_x = float(tile.grid_x * tile.tile_size)
    origin_y = float(tile.grid_y * tile.tile_size)
    positions = np.empty((mesh_side, mesh_side, 3), dtype=np.float64)
    positions[:, :, 0] = origin_x + coords[None, :]
    positions[:, :, 1] = origin_y + coords[:, None]
    positions[:, :, 2] = dense_height

    indices = _indices(mesh_side)
    weights = _resample_weights(
        tile.weights,
        coords,
        tile.material_params,
        len(tile.material_params),
    )
    if cancel_token is not None:
        cancel_token.check(f"{tile.tile_id}:weights")

    instance_records, instance_stats = _instances(tile, params)
    bounds = _bounds(tile, positions, instance_records, instance_stats)
    truth_check = _truth_check(
        tile,
        positions,
        weights,
        subdivisions,
        instance_stats["accepted"],
    )
    if verify_truth and not truth_check["ok"]:
        raise PipelineError(
            E_TRUTH,
            "baked output does not match analytic truth within tolerance",
            tile_id=tile.tile_id,
            details=truth_check,
        )
    if cancel_token is not None:
        cancel_token.check(f"{tile.tile_id}:instances")

    positions_bytes = np.ascontiguousarray(positions, dtype="<f4").tobytes(order="C")
    normals_bytes = np.ascontiguousarray(normals, dtype="<f4").tobytes(order="C")
    indices_bytes = np.ascontiguousarray(indices, dtype="<u4").tobytes(order="C")
    weights_bytes = np.ascontiguousarray(weights, dtype="u1").tobytes(order="C")
    instances_bytes = np.ascontiguousarray(instance_records, dtype="<f4").tobytes(order="C")
    bounds_bytes = struct.pack("<6f", *bounds)
    artifacts = {
        "positions.bin": positions_bytes,
        "normals.bin": normals_bytes,
        "indices.bin": indices_bytes,
        "weights.bin": weights_bytes,
        "instances.bin": instances_bytes,
        "bounds.bin": bounds_bytes,
    }
    artifact_hashes = {name: sha256_bytes(artifacts[name]) for name in ARTIFACT_ORDER}
    triangle_count = int(indices.size // 3)
    block_manifest = {
        "format": BLOCK_FORMAT,
        "tile_id": tile.tile_id,
        "grid": [tile.grid_x, tile.grid_y],
        "cache_key": cache_key,
        "content_hash": tile.content_hash,
        "bake_version": params.bake_version,
        "bake_params": params.output_dict(),
        "dependencies": list(dependencies),
        "source": {
            "tile_size": tile.tile_size,
            "height_shape": list(tile.height.shape),
            "weights_shape": list(tile.weights.shape),
            "material_params": {
                name: _json_float(tile.material_params[name])
                for name in sorted(tile.material_params)
            },
        },
        "mesh": {
            "subdivisions": subdivisions,
            "vertex_count": vertex_count,
            "triangle_count": triangle_count,
        },
        "instance_count": int(instance_stats["accepted"]),
        "instance_rejected": int(instance_stats["rejected"]),
        "bounds": [_json_float(value) for value in bounds],
        "artifacts": artifact_hashes,
        "truth_check": truth_check,
    }
    block_manifest_bytes = canonical_json_bytes(block_manifest)
    if cancel_token is not None:
        cancel_token.check(f"{tile.tile_id}:finalize")
    return BakedTile(
        tile_id=tile.tile_id,
        cache_key=cache_key,
        content_hash=tile.content_hash,
        bake_version=params.bake_version,
        dependencies=dependencies,
        artifacts=artifacts,
        artifact_hashes=artifact_hashes,
        block_manifest=block_manifest,
        block_manifest_bytes=block_manifest_bytes,
        vertex_count=vertex_count,
        triangle_count=triangle_count,
        instance_count=int(instance_stats["accepted"]),
        bounds=bounds,
        truth_check=truth_check,
    )


def _resample_height(
    source: np.ndarray,
    coords: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    max_coord = source.shape[0] - 1
    x0 = np.minimum(np.floor(coords).astype(np.int64), max_coord - 1)
    fx = coords - x0
    rows0 = x0[:, None]
    cols0 = x0[None, :]
    top = source[rows0, cols0] * (1.0 - fx[None, :]) + source[rows0, cols0 + 1] * fx[None, :]
    bottom = (
        source[rows0 + 1, cols0] * (1.0 - fx[None, :]) + source[rows0 + 1, cols0 + 1] * fx[None, :]
    )
    dense = top * (1.0 - fx[:, None]) + bottom * fx[:, None]
    return dense, fx


def _resample_weights(
    source: np.ndarray,
    coords: np.ndarray,
    material_params: Mapping[str, float],
    layer_count: int,
) -> np.ndarray:
    max_coord = source.shape[0] - 1
    x0 = np.minimum(np.floor(coords).astype(np.int64), max_coord - 1)
    fx = coords - x0
    rows0 = x0[:, None]
    cols0 = x0[None, :]
    source_f = source.astype(np.float64)
    top = (
        source_f[rows0, cols0, :] * (1.0 - fx[None, :, None])
        + source_f[rows0, cols0 + 1, :] * fx[None, :, None]
    )
    bottom = (
        source_f[rows0 + 1, cols0, :] * (1.0 - fx[None, :, None])
        + source_f[rows0 + 1, cols0 + 1, :] * fx[None, :, None]
    )
    dense = top * (1.0 - fx[:, None, None]) + bottom * fx[:, None, None]
    params = np.asarray(
        [float(material_params[name]) for name in material_params], dtype=np.float64
    )
    # Dict iteration order is insertion order, which follows manifest material order.
    dense = dense * params[None, None, :]
    dense = np.floor(np.clip(dense, 0.0, 255.0) + 0.5).astype(np.uint8)
    rgba = np.zeros((dense.shape[0], dense.shape[1], 4), dtype=np.uint8)
    rgba[:, :, :layer_count] = dense
    return rgba


def _normals(
    dense_height: np.ndarray,
    tile: TerrainTile,
    subdivisions: int,
    neighbor_heights: Mapping[str, np.ndarray],
    strength: float,
) -> tuple[np.ndarray, np.ndarray]:
    step = 1.0 / float(subdivisions)
    dzdy, dzdx = np.gradient(dense_height, step, step, edge_order=1)
    side_count = dense_height.shape[0]
    coords = np.arange(side_count, dtype=np.float64) / float(subdivisions)
    east = neighbor_heights.get("east")
    if east is not None:
        east_values = _sample_grid(east, np.full(side_count, step), coords)
        dzdx[:, -1] = (east_values - dense_height[:, -2]) / (2.0 * step)
    south = neighbor_heights.get("south")
    if south is not None:
        south_values = _sample_grid(south, coords, np.full(side_count, step))
        dzdy[-1, :] = (south_values - dense_height[-2, :]) / (2.0 * step)
    return dzdx * strength, dzdy * strength


def _sample_grid(source: np.ndarray, x: np.ndarray, y: np.ndarray) -> np.ndarray:
    max_coord = source.shape[0] - 1
    xs = np.clip(np.asarray(x, dtype=np.float64), 0.0, float(max_coord))
    ys = np.clip(np.asarray(y, dtype=np.float64), 0.0, float(max_coord))
    x0 = np.minimum(np.floor(xs).astype(np.int64), max_coord - 1)
    y0 = np.minimum(np.floor(ys).astype(np.int64), max_coord - 1)
    fx = xs - x0
    fy = ys - y0
    top = source[y0, x0] * (1.0 - fx) + source[y0, x0 + 1] * fx
    bottom = source[y0 + 1, x0] * (1.0 - fx) + source[y0 + 1, x0 + 1] * fx
    return top * (1.0 - fy) + bottom * fy


def _sample_weights_scalar(
    source: np.ndarray,
    x: float,
    y: float,
) -> np.ndarray:
    max_coord = source.shape[0] - 1
    x = min(max(float(x), 0.0), float(max_coord))
    y = min(max(float(y), 0.0), float(max_coord))
    x0 = min(math.floor(x), max_coord - 1)
    y0 = min(math.floor(y), max_coord - 1)
    fx = x - x0
    fy = y - y0
    top = (
        source[y0, x0].astype(np.float64) * (1.0 - fx) + source[y0, x0 + 1].astype(np.float64) * fx
    )
    bottom = (
        source[y0 + 1, x0].astype(np.float64) * (1.0 - fx)
        + source[y0 + 1, x0 + 1].astype(np.float64) * fx
    )
    return (top * (1.0 - fy) + bottom * fy) / 255.0


def _indices(mesh_side: int) -> np.ndarray:
    rows, cols = np.mgrid[0 : mesh_side - 1, 0 : mesh_side - 1]
    a = (rows * mesh_side + cols).astype(np.uint32)
    b = a + 1
    c = a + np.uint32(mesh_side)
    d = c + 1
    triangles = np.stack(
        [
            np.stack([a, b, c], axis=-1),
            np.stack([b, d, c], axis=-1),
        ],
        axis=-2,
    )
    return np.ascontiguousarray(triangles.reshape(-1), dtype=np.uint32)


def _instances(tile: TerrainTile, params: BakeParams) -> tuple[np.ndarray, dict[str, int]]:
    material_index = {name: index for index, name in enumerate(tile.material_params)}
    accepted = 0
    rejected = 0
    records: list[tuple[float, float, float, float, float, float, float, float]] = []
    for point in tile.scatter:
        sampled_weights = _sample_weights_scalar(tile.weights, point.x, point.y)
        layer_index = material_index[point.layer]
        if not (0.0 <= point.x < tile.tile_size and 0.0 <= point.y < tile.tile_size):
            rejected += 1
            continue
        if sampled_weights[layer_index] < params.min_layer_weight:
            rejected += 1
            continue
        height = float(_sample_grid(tile.height, np.asarray([point.x]), np.asarray([point.y]))[0])
        records.append(
            (
                float(tile.grid_x * tile.tile_size) + point.x,
                float(tile.grid_y * tile.tile_size) + point.y,
                height,
                point.scale,
                point.rotation,
                float(KIND_CODES[point.kind]),
                float(layer_index),
                point.variant,
            )
        )
        accepted += 1
    array = np.asarray(records, dtype="<f4").reshape((-1, 8))
    return array, {"accepted": accepted, "rejected": rejected}


def _bounds(
    tile: TerrainTile,
    positions: np.ndarray,
    instances: np.ndarray,
    instance_stats: Mapping[str, int],
) -> tuple[float, float, float, float, float, float]:
    min_x = float(positions[:, :, 0].min())
    max_x = float(positions[:, :, 0].max())
    min_y = float(positions[:, :, 1].min())
    max_y = float(positions[:, :, 1].max())
    min_z = float(positions[:, :, 2].min())
    max_z = float(positions[:, :, 2].max())
    if instances.size:
        min_x = min(min_x, float(instances[:, 0].min() - instances[:, 3].max()))
        max_x = max(max_x, float(instances[:, 0].max() + instances[:, 3].max()))
        min_y = min(min_y, float(instances[:, 1].min() - instances[:, 3].max()))
        max_y = max(max_y, float(instances[:, 1].max() + instances[:, 3].max()))
        min_z = min(min_z, float(instances[:, 2].min()))
        max_z = max(max_z, float(instances[:, 2].max() + 3.0 * instances[:, 3].max()))
    return (
        _float32_value(min_x),
        _float32_value(min_y),
        _float32_value(min_z),
        _float32_value(max_x),
        _float32_value(max_y),
        _float32_value(max_z),
    )


def _truth_check(
    tile: TerrainTile,
    positions: np.ndarray,
    weights: np.ndarray,
    subdivisions: int,
    instance_count: int,
) -> dict[str, Any]:
    height_error = 0.0
    for sample in tile.truth.height_samples:
        baked_value = float(positions[sample.y * subdivisions, sample.x * subdivisions, 2])
        height_error = max(height_error, abs(baked_value - sample.height))
    material_names = list(tile.material_params)
    coverage_error = 0.0
    baked_coverage: dict[str, float] = {}
    for layer, name in enumerate(material_names):
        value = float(weights[:, :, layer].astype(np.float64).mean() / 255.0)
        baked_coverage[name] = value
        expected = float(tile.truth.material_coverage[name])
        coverage_error = max(coverage_error, abs(value - expected))
    scatter_ok = int(instance_count) == int(tile.truth.scatter_count)
    ok = height_error <= HEIGHT_TOLERANCE and coverage_error <= COVERAGE_TOLERANCE and scatter_ok
    return {
        "ok": bool(ok),
        "height_max_abs_error": _json_float(height_error),
        "height_tolerance": HEIGHT_TOLERANCE,
        "coverage_max_abs_error": _json_float(coverage_error),
        "coverage_tolerance": COVERAGE_TOLERANCE,
        "coverage": {name: _json_float(baked_coverage[name]) for name in sorted(baked_coverage)},
        "scatter_expected": int(tile.truth.scatter_count),
        "scatter_actual": int(instance_count),
        "scatter_ok": bool(scatter_ok),
    }


def _float32_value(value: float) -> float:
    return float(np.float32(value))


def _json_float(value: float) -> float:
    numeric = float(value)
    if numeric == 0.0:
        return 0.0
    return numeric
