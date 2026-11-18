"""Seed-stable crackle and carved-relief lookup tables."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .math3d import sha256_array, smoothstep


_U64 = np.uint64
_MASK64 = _U64(0xFFFFFFFFFFFFFFFF)


def _mix64(values: np.ndarray) -> np.ndarray:
    x = np.asarray(values, dtype=np.uint64)
    with np.errstate(over="ignore"):
        x = (x ^ (x >> _U64(30))) * _U64(0xBF58476D1CE4E5B9)
        x = (x ^ (x >> _U64(27))) * _U64(0x94D049BB133111EB)
        x = x ^ (x >> _U64(31))
    return x & _MASK64


def _hash01(seed: int, x: np.ndarray | int, y: np.ndarray | int, salt: int = 0) -> np.ndarray:
    xa = np.asarray(x, dtype=np.int64).astype(np.uint64)
    ya = np.asarray(y, dtype=np.int64).astype(np.uint64)
    h = _U64(int(seed) & 0xFFFFFFFFFFFFFFFF)
    h = h ^ (xa * _U64(0x9E3779B185EBCA87))
    h = h ^ (ya * _U64(0xC2B2AE3D27D4EB4F))
    h = h ^ _U64(int(salt) & 0xFFFFFFFFFFFFFFFF)
    mixed = _mix64(h)
    return (mixed >> _U64(11)).astype(np.float64) * (1.0 / 9007199254740992.0)


def _smooth_fraction(value: np.ndarray) -> np.ndarray:
    return value * value * (3.0 - 2.0 * value)


def _value_noise(seed: int, u: np.ndarray, v: np.ndarray, frequency: float, salt: int) -> np.ndarray:
    gx = u * frequency
    gy = v * frequency
    ix = np.floor(gx).astype(np.int64)
    iy = np.floor(gy).astype(np.int64)
    fx = _smooth_fraction(gx - ix)
    fy = _smooth_fraction(gy - iy)
    n00 = _hash01(seed, ix, iy, salt)
    n10 = _hash01(seed, ix + 1, iy, salt)
    n01 = _hash01(seed, ix, iy + 1, salt)
    n11 = _hash01(seed, ix + 1, iy + 1, salt)
    nx0 = n00 * (1.0 - fx) + n10 * fx
    nx1 = n01 * (1.0 - fx) + n11 * fx
    return nx0 * (1.0 - fy) + nx1 * fy


def _worley_distances(batch: Any, seed: int, resolution: tuple[int, int], cell_size: float):
    resx, resy = resolution
    xs = np.arange(resx, dtype=np.float64) + 0.5
    ys = np.arange(resy, dtype=np.float64) + 0.5
    grid_x, grid_y = np.meshgrid(xs / cell_size, ys / cell_size)
    cell_x = np.floor(grid_x).astype(np.int64)
    cell_y = np.floor(grid_y).astype(np.int64)
    distances = []
    for dy in (-1, 0, 1):
        for dx in (-1, 0, 1):
            cx = cell_x + dx
            cy = cell_y + dy
            px = cx + 0.10 + 0.80 * _hash01(seed, cx, cy, 11)
            py = cy + 0.10 + 0.80 * _hash01(seed, cx, cy, 17)
            distances.append((grid_x - px) ** 2 + (grid_y - py) ** 2)
    packed = np.stack(distances, axis=0)
    nearest = np.partition(packed, kth=(0, 1, 2), axis=0)
    f1 = nearest[0]
    f2 = nearest[1]
    f3 = nearest[2]
    return f1, f2, f3


def _box_blur_3x3(values: np.ndarray) -> np.ndarray:
    padded = np.pad(values, 1, mode="edge")
    total = np.zeros_like(values)
    for dy in range(3):
        for dx in range(3):
            total += padded[dy : dy + values.shape[0], dx : dx + values.shape[1]]
    return total / 9.0


def _raw_crackle_tables(seed: int, resolution: tuple[int, int]) -> tuple[np.ndarray, np.ndarray]:
    f1, f2, f3 = _worley_distances(None, seed, resolution, cell_size=14.0)
    raw = np.exp(-np.square((f2 - f1) / 0.060))
    junction = (
        (f2 - f1 < 0.085)
        & (f3 - f1 < 0.085)
        & (raw > 0.22)
    )
    return raw.astype(np.float64), junction


def _lexicographic_hash(seed: int, ix: int, iy: int, salt: int) -> float:
    mask = 0xFFFFFFFFFFFFFFFF
    hash_value = (
        (int(seed) & mask)
        ^ ((int(ix) & mask) * 0x9E3779B185EBCA87 & mask)
        ^ ((int(iy) & mask) * 0xC2B2AE3D27D4EB4F & mask)
        ^ (int(salt) & mask)
    ) & mask
    mixed = _mix64(np.asarray([hash_value], dtype=np.uint64))
    return float(int(mixed[0]) >> 11) * (1.0 / 9007199254740992.0)


def _raw_crackle_scalar(seed: int, resolution: tuple[int, int], x: int, y: int):
    resx, resy = resolution
    if x < 0 or y < 0 or x >= resx or y >= resy:
        raise ValueError("crackle sample coordinate is outside the layer resolution")
    gx = (x + 0.5) / 14.0
    gy = (y + 0.5) / 14.0
    cx = int(np.floor(gx))
    cy = int(np.floor(gy))
    distances: list[float] = []
    for dy in (-1, 0, 1):
        for dx in (-1, 0, 1):
            cell_x = cx + dx
            cell_y = cy + dy
            px = cell_x + 0.10 + 0.80 * _lexicographic_hash(seed, cell_x, cell_y, 11)
            py = cell_y + 0.10 + 0.80 * _lexicographic_hash(seed, cell_x, cell_y, 17)
            distances.append((gx - px) ** 2 + (gy - py) ** 2)
    distances.sort()
    f1, f2, f3 = distances[0], distances[1], distances[2]
    raw = float(np.exp(-((f2 - f1) / 0.060) ** 2))
    junction = (f2 - f1 < 0.085) and (f3 - f1 < 0.085) and (raw > 0.22)
    return raw, junction


def sample_crackle(seed: int, resolution: tuple[int, int], x: int, y: int) -> float:
    """Independently recompute one suppressed crackle texel from seed and resolution."""

    raw, junction = _raw_crackle_scalar(seed, resolution, x, y)
    if not junction:
        return raw
    resx, resy = resolution
    total = 0.0
    for dy in (-1, 0, 1):
        for dx in (-1, 0, 1):
            sx = min(max(x + dx, 0), resx - 1)
            sy = min(max(y + dy, 0), resy - 1)
            neighbor, _ = _raw_crackle_scalar(seed, resolution, sx, sy)
            total += neighbor
    blur = total / 9.0
    return 0.25 * raw + 0.75 * blur


def _make_carve_table(seed: int, resolution: tuple[int, int], carve_depth: float) -> np.ndarray:
    resx, resy = resolution
    u = (np.arange(resx, dtype=np.float64) + 0.5) / resx
    v = (np.arange(resy, dtype=np.float64) + 0.5) / resy
    uu, vv = np.meshgrid(u, v)
    dx = uu - 0.5
    dy = vv - 0.5
    radius = np.sqrt(dx * dx + dy * dy) * 2.0
    theta = np.arctan2(dy, dx)
    phase = _hash01(seed, 7, 31, 23) * (2.0 * np.pi)
    noise = _value_noise(seed, uu, vv, 7.0, 41)
    fine = _value_noise(seed, uu, vv, 19.0, 43)
    ring = smoothstep(0.10, 0.31, radius) * (1.0 - smoothstep(0.84, 1.02, radius))
    petals = 0.5 + 0.5 * np.cos(8.0 * theta + phase + 0.35 * (noise - 0.5))
    flutes = 0.5 + 0.5 * np.cos(
        (6.0 * radius + 0.22 * (fine - 0.5)) * 2.0 * np.pi + phase * 0.5
    )
    relief = (
        0.50
        + 0.20 * petals * ring
        + 0.17 * (flutes - 0.5) * ring
        + (0.10 * noise + 0.05 * fine - 0.075) * ring
        - 0.08 * smoothstep(0.72, 0.96, radius)
    )
    relief = 0.5 + (relief - 0.5) * max(carve_depth, 0.0)
    return np.clip(relief, 0.0, 1.0).astype(np.float64)


@dataclass(frozen=True)
class LayerTextures:
    crackle: np.ndarray
    carve: np.ndarray
    aperture: np.ndarray
    junction_mask: np.ndarray
    crackle_digest: str
    carve_digest: str
    aperture_digest: str

    @property
    def junction_samples(self) -> int:
        return int(np.count_nonzero(self.junction_mask))


def generate_layer_textures(
    layer: Any,
    resolution: tuple[int, int] | None = None,
) -> LayerTextures:
    """Generate byte-stable crackle and carve tables for one layer."""

    if resolution is None:
        resolution = tuple(int(v) for v in layer.texture_resolution)
    raw, junction = _raw_crackle_tables(int(layer.seed), resolution)
    blur = _box_blur_3x3(raw)
    crackle = np.where(junction, 0.25 * raw + 0.75 * blur, raw)
    crackle = np.clip(crackle, 0.0, 1.0)
    carve = _make_carve_table(
        int(layer.seed),
        resolution,
        float(getattr(layer, "carve_depth", 1.0)),
    )
    resx, resy = resolution
    u = (np.arange(resx, dtype=np.float64) + 0.5) / resx
    v = (np.arange(resy, dtype=np.float64) + 0.5) / resy
    uu, vv = np.meshgrid(u, v)
    radius = np.sqrt((uu - 0.5) ** 2 + (vv - 0.5) ** 2) * 2.0
    aperture = np.clip((0.60 - carve) / 0.27, 0.0, 1.0)
    aperture *= 1.0 - smoothstep(0.93, 1.02, radius)
    return LayerTextures(
        crackle=np.ascontiguousarray(crackle),
        carve=np.ascontiguousarray(carve),
        aperture=np.ascontiguousarray(aperture),
        junction_mask=np.ascontiguousarray(junction),
        crackle_digest=sha256_array(crackle),
        carve_digest=sha256_array(carve),
        aperture_digest=sha256_array(aperture),
    )


def bilinear_sample(
    table: np.ndarray,
    u: np.ndarray | float,
    v: np.ndarray | float,
    wrap_u: bool = False,
) -> np.ndarray:
    """Bilinear sample with deterministic edge handling."""

    resy, resx = table.shape
    ua = np.asarray(u, dtype=np.float64)
    va = np.asarray(v, dtype=np.float64)
    if wrap_u:
        ua = ua - np.floor(ua)
    else:
        ua = np.clip(ua, 0.0, 1.0)
    va = np.clip(va, 0.0, 1.0)
    x = ua * (resx - 1)
    y = va * (resy - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, resx - 1)
    y1 = np.minimum(y0 + 1, resy - 1)
    fx = x - x0
    fy = y - y0
    v00 = table[y0, x0]
    v10 = table[y0, x1]
    v01 = table[y1, x0]
    v11 = table[y1, x1]
    top = v00 * (1.0 - fx) + v10 * fx
    bottom = v01 * (1.0 - fx) + v11 * fx
    return top * (1.0 - fy) + bottom * fy
