"""Density-field validation, block indexing, and trilinear sampling."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .errors import scene_invalid
from .util import sha256_bytes


@dataclass(frozen=True)
class DensityBlock:
    field_name: str
    index: tuple[int, int, int]
    origin: tuple[int, int, int]
    shape: tuple[int, int, int]
    offset: int
    count: int
    hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": list(self.index),
            "origin": list(self.origin),
            "shape": list(self.shape),
            "offset": self.offset,
            "count": self.count,
            "hash": self.hash,
        }


@dataclass
class DensityField:
    name: str
    grid: tuple[int, int, int]
    bounds_min: np.ndarray
    bounds_max: np.ndarray
    default: float
    version: int
    blocks: tuple[DensityBlock, ...]
    values: np.ndarray

    def sample(self, point: np.ndarray) -> float:
        point = np.asarray(point, dtype=np.float64)
        span = self.bounds_max - self.bounds_min
        if np.any(span <= 0):
            return float(self.default)
        grid_pos = (point - self.bounds_min) / span * (np.asarray(self.grid) - 1)
        if np.any(grid_pos < 0.0) or np.any(grid_pos > np.asarray(self.grid) - 1):
            return float(self.default)
        base = np.floor(grid_pos).astype(np.int64)
        frac = grid_pos - base
        maximum = np.asarray(self.grid) - 2
        base = np.minimum(np.maximum(base, 0), maximum)
        frac = np.clip(frac, 0.0, 1.0)
        x0, y0, z0 = (int(value) for value in base)
        fx, fy, fz = (float(value) for value in frac)
        values = self.values
        c00 = values[x0, y0, z0] * (1 - fx) + values[x0 + 1, y0, z0] * fx
        c01 = values[x0, y0, z0 + 1] * (1 - fx) + values[x0 + 1, y0, z0 + 1] * fx
        c10 = values[x0, y0 + 1, z0] * (1 - fx) + values[x0 + 1, y0 + 1, z0] * fx
        c11 = values[x0, y0 + 1, z0 + 1] * (1 - fx) + values[x0 + 1, y0 + 1, z0 + 1] * fx
        c0 = c00 * (1 - fy) + c10 * fy
        c1 = c01 * (1 - fy) + c11 * fy
        return float(c0 * (1 - fz) + c1 * fz)

    def physical_bounds_for_blocks(
        self, block_indices: Iterable[tuple[int, int, int]]
    ) -> tuple[np.ndarray, np.ndarray] | None:
        selected = set(tuple(value) for value in block_indices)
        minima: list[np.ndarray] = []
        maxima: list[np.ndarray] = []
        cell = (self.bounds_max - self.bounds_min) / (np.asarray(self.grid) - 1)
        for block in self.blocks:
            if block.index not in selected:
                continue
            origin = np.asarray(block.origin, dtype=np.float64)
            end = origin + np.asarray(block.shape) - 1
            minima.append(self.bounds_min + origin * cell)
            maxima.append(self.bounds_min + end * cell)
        if not minima:
            return None
        return np.min(np.stack(minima), axis=0), np.max(np.stack(maxima), axis=0)

    def block_for_grid_point(self, index: np.ndarray) -> tuple[int, int, int] | None:
        point = np.asarray(index, dtype=np.int64)
        for block in self.blocks:
            start = np.asarray(block.origin)
            end = start + np.asarray(block.shape)
            if np.all(point >= start) and np.all(point < end):
                return block.index
        return None


def _as_int_triplet(
    value: Any, path: str, minimum: int | None = None
) -> tuple[int, int, int]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise scene_invalid(path, "must be a three-element integer array")
    result: list[int] = []
    for index, item in enumerate(value):
        if isinstance(item, bool) or not isinstance(item, int):
            raise scene_invalid(f"{path}[{index}]", "must be an integer")
        if minimum is not None and item < minimum:
            raise scene_invalid(f"{path}[{index}]", f"must be >= {minimum}")
        result.append(item)
    return tuple(result)


def build_density_fields(
    raw_fields: Any, payload: bytes
) -> dict[str, DensityField]:
    path = "$.density_fields"
    if not isinstance(raw_fields, list) or not raw_fields:
        raise scene_invalid(path, "must be a non-empty array")
    result: dict[str, DensityField] = {}
    consumed_ranges: list[tuple[int, int, str]] = []
    for field_index, raw in enumerate(raw_fields):
        fp = f"{path}[{field_index}]"
        if not isinstance(raw, dict):
            raise scene_invalid(fp, "must be an object")
        name = raw.get("name")
        if not isinstance(name, str) or not name:
            raise scene_invalid(f"{fp}.name", "must be a non-empty string")
        if name in result:
            raise scene_invalid(f"{fp}.name", f"duplicate density field {name!r}")
        grid = _as_int_triplet(raw.get("grid"), f"{fp}.grid", minimum=2)
        bounds_min = _finite_vector(raw.get("bounds_min"), f"{fp}.bounds_min")
        bounds_max = _finite_vector(raw.get("bounds_max"), f"{fp}.bounds_max")
        if np.any(bounds_max <= bounds_min):
            raise scene_invalid(f"{fp}.bounds_max", "must be strictly greater than bounds_min")
        default = raw.get("default", 0.0)
        if not isinstance(default, (int, float)) or isinstance(default, bool):
            raise scene_invalid(f"{fp}.default", "must be numeric")
        if not math.isfinite(float(default)) or default < 0:
            raise scene_invalid(f"{fp}.default", "must be finite and non-negative")
        version = raw.get("version")
        if isinstance(version, bool) or not isinstance(version, int) or version < 0:
            raise scene_invalid(f"{fp}.version", "must be a non-negative integer")
        raw_blocks = raw.get("blocks")
        if not isinstance(raw_blocks, list) or not raw_blocks:
            raise scene_invalid(f"{fp}.blocks", "must be a non-empty array")
        values = np.full(grid, float(default), dtype=np.float32)
        blocks: list[DensityBlock] = []
        seen_indices: set[tuple[int, int, int]] = set()
        for block_index, raw_block in enumerate(raw_blocks):
            bp = f"{fp}.blocks[{block_index}]"
            if not isinstance(raw_block, dict):
                raise scene_invalid(bp, "must be an object")
            index = _as_int_triplet(raw_block.get("index"), f"{bp}.index", minimum=0)
            origin = _as_int_triplet(raw_block.get("origin"), f"{bp}.origin", minimum=0)
            shape = _as_int_triplet(raw_block.get("shape"), f"{bp}.shape", minimum=1)
            count = raw_block.get("count")
            offset = raw_block.get("offset")
            block_hash = raw_block.get("hash")
            if isinstance(count, bool) or not isinstance(count, int) or count <= 0:
                raise scene_invalid(f"{bp}.count", "must be a positive integer")
            if count != shape[0] * shape[1] * shape[2]:
                raise scene_invalid(f"{bp}.count", "must equal the product of shape")
            if isinstance(offset, bool) or not isinstance(offset, int) or offset < 0:
                raise scene_invalid(f"{bp}.offset", "must be a non-negative integer")
            if offset % 4:
                raise scene_invalid(f"{bp}.offset", "must be 4-byte aligned")
            if not isinstance(block_hash, str) or len(block_hash) != 64:
                raise scene_invalid(f"{bp}.hash", "must be a SHA-256 hex string")
            try:
                int(block_hash, 16)
            except ValueError as exc:
                raise scene_invalid(f"{bp}.hash", "must be a SHA-256 hex string") from exc
            if index in seen_indices:
                raise scene_invalid(f"{bp}.index", "duplicate density block index")
            seen_indices.add(index)
            for axis in range(3):
                if origin[axis] + shape[axis] > grid[axis]:
                    raise scene_invalid(
                        f"{bp}.origin",
                        f"block exceeds density grid on axis {axis}",
                    )
                axis_blocks = int(math.ceil(grid[axis] / float(shape[axis])))
                if index[axis] >= axis_blocks:
                    raise scene_invalid(
                        f"{bp}.index[{axis}]",
                        f"block index exceeds the field's {axis_blocks}-block axis extent",
                    )
            end = offset + count * 4
            if end > len(payload):
                raise scene_invalid(
                    f"{bp}.offset", "block payload exceeds the declared binary payload"
                )
            for other_start, other_end, other_path in consumed_ranges:
                if offset < other_end and end > other_start:
                    raise scene_invalid(
                        f"{bp}.offset", f"block payload overlaps {other_path}"
                    )
            consumed_ranges.append((offset, end, bp))
            raw_values = payload[offset:end]
            actual_hash = sha256_bytes(raw_values)
            if actual_hash != block_hash.lower():
                raise scene_invalid(
                    f"{bp}.hash",
                    "density block hash mismatch",
                    expected=block_hash.lower(),
                    actual=actual_hash,
                )
            data = np.frombuffer(raw_values, dtype="<f4").reshape(shape, order="C")
            if not np.all(np.isfinite(data)) or np.any(data < 0):
                raise scene_invalid(f"{bp}.hash", "density values must be finite and non-negative")
            sx, sy, sz = origin
            ex, ey, ez = sx + shape[0], sy + shape[1], sz + shape[2]
            values[sx:ex, sy:ey, sz:ez] = data
            blocks.append(
                DensityBlock(name, index, origin, shape, offset, count, block_hash.lower())
            )
        result[name] = DensityField(
            name=name,
            grid=grid,
            bounds_min=bounds_min,
            bounds_max=bounds_max,
            default=float(default),
            version=version,
            blocks=tuple(blocks),
            values=values,
        )
    return result


def _finite_vector(value: Any, path: str) -> np.ndarray:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise scene_invalid(path, "must be a three-element numeric array")
    result = []
    for index, item in enumerate(value):
        if not isinstance(item, (int, float)) or isinstance(item, bool):
            raise scene_invalid(f"{path}[{index}]", "must be numeric")
        if not math.isfinite(float(item)):
            raise scene_invalid(f"{path}[{index}]", "must be finite")
        result.append(float(item))
    return np.asarray(result, dtype=np.float64)
