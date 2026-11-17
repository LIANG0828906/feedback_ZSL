from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache
import hashlib

import numpy as np

from .scene import Layer


MASK64 = (1 << 64) - 1
FLOAT53 = 1.0 / float(1 << 53)


@dataclass(frozen=True)
class WeaveTexture:
    seed: int
    resolution: tuple[int, int]
    transmission: np.ndarray
    color: np.ndarray
    digest: str


def _u64(value: int) -> np.uint64:
    return np.uint64(value & MASK64)


def _mix64(value: np.ndarray) -> np.ndarray:
    value = value ^ (value >> np.uint64(30))
    value = value * np.uint64(0xBF58476D1CE4E5B9)
    value = value ^ (value >> np.uint64(27))
    value = value * np.uint64(0x94D049BB133111EB)
    value = value ^ (value >> np.uint64(31))
    return value


def _hash_grid(
    seed: int,
    x: np.ndarray,
    y: np.ndarray,
    salt: int,
) -> np.ndarray:
    x64 = np.asarray(x, dtype=np.uint64)
    y64 = np.asarray(y, dtype=np.uint64)
    with np.errstate(over="ignore"):
        value = _u64(seed) ^ _u64(salt)
        value = value ^ (x64 * np.uint64(0x9E3779B185EBCA87))
        value = value ^ (y64 * np.uint64(0xC2B2AE3D27D4EB4F))
        value = value + np.uint64(0x165667B19E3779F9)
        return _mix64(value)


def _unit_float_bits(bits: np.ndarray) -> np.ndarray:
    return (bits >> np.uint64(11)).astype(np.float64) * FLOAT53


def _value_noise(
    seed: int,
    width: int,
    height: int,
    grid_width: int,
    grid_height: int,
    salt: int,
) -> np.ndarray:
    gx = np.arange(grid_width, dtype=np.uint64)
    gy = np.arange(grid_height, dtype=np.uint64)
    grid_x, grid_y = np.meshgrid(gx, gy)
    coarse = _unit_float_bits(_hash_grid(seed, grid_x, grid_y, salt))

    x = np.linspace(0.0, grid_width - 1, width, endpoint=False)
    y = np.linspace(0.0, grid_height - 1, height, endpoint=False)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    fx = x - x0
    fy = y - y0
    x1 = np.minimum(x0 + 1, grid_width - 1)
    y1 = np.minimum(y0 + 1, grid_height - 1)
    fx = fx * fx * (3.0 - 2.0 * fx)
    fy = fy * fy * (3.0 - 2.0 * fy)

    v00 = coarse[y0[:, None], x0[None, :]]
    v10 = coarse[y0[:, None], x1[None, :]]
    v01 = coarse[y1[:, None], x0[None, :]]
    v11 = coarse[y1[:, None], x1[None, :]]
    top = v00 * (1.0 - fx)[None, :] + v10 * fx[None, :]
    bottom = v01 * (1.0 - fx)[None, :] + v11 * fx[None, :]
    return top * (1.0 - fy)[:, None] + bottom * fy[:, None]


@lru_cache(maxsize=64)
def generate_weave_texture(seed: int, resolution: tuple[int, int]) -> WeaveTexture:
    """Generate a seed-only yarn/weave sampling table.

    The table is a pure function of (seed, resolution). It uses an explicit
    integer hash rather than NumPy's global RNG so traversal order and prior
    calls cannot affect the bytes.
    """
    width, height = int(resolution[0]), int(resolution[1])
    x = np.arange(width, dtype=np.uint64)
    y = np.arange(height, dtype=np.uint64)
    xx, yy = np.meshgrid(x, y)

    thread_x = max(2, width // 24)
    thread_y = max(2, height // 24)
    ix = xx // np.uint64(thread_x)
    iy = yy // np.uint64(thread_y)
    thread_hash = _unit_float_bits(_hash_grid(seed, ix, iy, 0x51A7))
    fine_hash = _unit_float_bits(_hash_grid(seed, xx, yy, 0x9E11))
    low = _value_noise(seed, width, height, 9, 7, 0x4C4F57)

    phase_x = (xx % np.uint64(thread_x)).astype(np.float64) / float(thread_x)
    phase_y = (yy % np.uint64(thread_y)).astype(np.float64) / float(thread_y)
    ridge_x = 1.0 - np.abs(phase_x * 2.0 - 1.0)
    ridge_y = 1.0 - np.abs(phase_y * 2.0 - 1.0)
    interlace = ((ix + iy) & np.uint64(1)).astype(np.float64)
    weave = (
        0.62
        + 0.18 * ridge_x
        + 0.18 * ridge_y
        + 0.10 * interlace
        + 0.16 * thread_hash
        + 0.08 * fine_hash
    )

    transmission = np.clip(0.56 + 0.52 * weave + 0.24 * (low - 0.5), 0.42, 1.25)
    transmission = transmission.astype(np.float64)

    channels = []
    for salt in (0xA12B, 0xB36D, 0xC58F):
        channel_hash = _unit_float_bits(_hash_grid(seed, xx, yy, salt))
        channel = 0.86 + 0.18 * (channel_hash - 0.5) + 0.11 * (low - 0.5)
        channel = channel + 0.035 * (weave - 0.85)
        channels.append(np.clip(channel, 0.66, 1.12))
    color = np.stack(channels, axis=2).astype(np.float64)

    digest = hashlib.sha256()
    digest.update(b"serene-weave-v1\0")
    digest.update(str(seed).encode("ascii"))
    digest.update(b"\0")
    digest.update(f"{width}x{height}".encode("ascii"))
    digest.update(b"\0")
    digest.update(np.ascontiguousarray(transmission, dtype="<f8").tobytes())
    digest.update(np.ascontiguousarray(color, dtype="<f8").tobytes())

    transmission.setflags(write=False)
    color.setflags(write=False)
    return WeaveTexture(
        seed=seed,
        resolution=(width, height),
        transmission=transmission,
        color=color,
        digest=digest.hexdigest(),
    )


def sample_weave(
    texture: WeaveTexture,
    layer: Layer,
    world_points: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    """Bilinearly sample a weave table using layer-local planar coordinates."""
    points = np.asarray(world_points, dtype=np.float64).reshape(-1, 3)
    if points.shape[0] == 0:
        return np.empty(0, dtype=np.float64), np.empty((0, 3), dtype=np.float64)

    scale = float(layer.material["texture_scale"])
    relative = points - layer.plane_point[None, :]
    u = (relative @ layer.basis_u) / scale
    v = (relative @ layer.basis_v) / scale
    width, height = texture.resolution
    px = u * width
    py = v * height
    x0 = np.floor(px).astype(np.int64)
    y0 = np.floor(py).astype(np.int64)
    fx = px - x0
    fy = py - y0
    x0 %= width
    y0 %= height
    x1 = (x0 + 1) % width
    y1 = (y0 + 1) % height

    def bilinear(table: np.ndarray) -> np.ndarray:
        v00 = table[y0, x0]
        v10 = table[y0, x1]
        v01 = table[y1, x0]
        v11 = table[y1, x1]
        if table.ndim == 2:
            top = v00 * (1.0 - fx) + v10 * fx
            bottom = v01 * (1.0 - fx) + v11 * fx
            return top * (1.0 - fy) + bottom * fy
        top = v00 * (1.0 - fx)[:, None] + v10 * fx[:, None]
        bottom = v01 * (1.0 - fx)[:, None] + v11 * fx[:, None]
        return top * (1.0 - fy)[:, None] + bottom * fy[:, None]

    return bilinear(texture.transmission), bilinear(texture.color)


def texture_preview_rgb(texture: WeaveTexture) -> np.ndarray:
    base = np.array([0.82, 0.55, 0.34], dtype=np.float64)
    trans = texture.transmission[:, :, None]
    rgb = np.clip(base[None, None, :] * texture.color * (0.55 + 0.45 * trans), 0.0, 1.0)
    return np.floor(rgb * 255.0 + 0.5).astype(np.uint8)
