"""Deterministic star-point tables and texture sampling."""

from __future__ import annotations

import hashlib
import struct
from dataclasses import dataclass

import numpy as np


def _hash_u64(seed: int, stream: str, index: int) -> int:
    payload = f"dome-star-v1|{seed}|{stream}|{index}".encode("utf-8")
    return int.from_bytes(hashlib.sha256(payload).digest()[:8], "little")


def _uniform(seed: int, stream: str, index: int) -> float:
    return (_hash_u64(seed, stream, index) >> 11) * (1.0 / 9007199254740992.0)


@dataclass(frozen=True)
class StarField:
    seed: int
    width: int
    height: int
    positions: np.ndarray
    brightness: np.ndarray
    radii: np.ndarray
    colors: np.ndarray
    texture: np.ndarray
    texture_digest: str

    @property
    def count(self) -> int:
        return int(len(self.brightness))


def generate_star_field(seed: int, width: int, height: int) -> StarField:
    """Generate a star table using only the declared integer seed and resolution."""
    if width <= 0 or height <= 0:
        raise ValueError("star texture resolution must be positive")
    base_count = max(24, min(768, (width * height) // 400))
    variation = _hash_u64(seed, "count", 0) % max(1, base_count // 6)
    count = base_count + variation
    positions = np.empty((count, 2), dtype=np.float64)
    brightness = np.empty(count, dtype=np.float64)
    radii = np.empty(count, dtype=np.float64)
    colors = np.empty((count, 3), dtype=np.float64)
    for index in range(count):
        margin = 0.012
        u = margin + (1.0 - 2.0 * margin) * _uniform(seed, "u", index)
        v = margin + (1.0 - 2.0 * margin) * _uniform(seed, "v", index)
        positions[index] = (u * (width - 1), v * (height - 1))
        brightness[index] = 0.10 + 0.90 * _uniform(seed, "brightness", index) ** 0.65
        radii[index] = 0.48 + 0.92 * _uniform(seed, "radius", index)
        warm = _uniform(seed, "color", index)
        colors[index] = (0.78 + 0.22 * warm, 0.84 + 0.16 * (1.0 - abs(warm - 0.5) * 2.0), 0.90 + 0.10 * (1.0 - warm))

    texture_float = np.zeros((height, width, 3), dtype=np.float64)
    yy, xx = np.mgrid[0:height, 0:width]
    for index in range(count):
        px, py = positions[index]
        radius = radii[index]
        reach = max(1.0, radius * 3.0)
        x0 = max(0, int(np.floor(px - reach)))
        x1 = min(width, int(np.ceil(px + reach)) + 1)
        y0 = max(0, int(np.floor(py - reach)))
        y1 = min(height, int(np.ceil(py + reach)) + 1)
        if x0 >= x1 or y0 >= y1:
            continue
        local_x = xx[y0:y1, x0:x1] - px
        local_y = yy[y0:y1, x0:x1] - py
        distance_sq = local_x * local_x + local_y * local_y
        support = (radius * 2.6) ** 2
        profile = np.clip(1.0 - distance_sq / support, 0.0, 1.0)
        profile = profile * profile * brightness[index]
        texture_float[y0:y1, x0:x1] += profile[..., None] * colors[index]
    texture_float = np.clip(texture_float, 0.0, 1.0)
    texture = np.rint(texture_float * 65535.0).astype(np.uint16)
    digest = hashlib.sha256(texture.astype("<u2", copy=False).tobytes()).hexdigest()
    return StarField(
        seed=seed,
        width=width,
        height=height,
        positions=positions,
        brightness=brightness,
        radii=radii,
        colors=colors,
        texture=texture,
        texture_digest=digest,
    )


def sample_star(field: StarField, u: np.ndarray | float, v: np.ndarray | float) -> np.ndarray:
    """Bilinear RGB sample from a quantized star table. UVs are clamped."""
    u_arr = np.clip(np.asarray(u, dtype=np.float64), 0.0, 1.0)
    v_arr = np.clip(np.asarray(v, dtype=np.float64), 0.0, 1.0)
    x = u_arr * (field.width - 1)
    y = v_arr * (field.height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, field.width - 1)
    y1 = np.minimum(y0 + 1, field.height - 1)
    fx = x - x0
    fy = y - y0
    texture = field.texture.astype(np.float64) * (1.0 / 65535.0)
    c00 = texture[y0, x0]
    c10 = texture[y0, x1]
    c01 = texture[y1, x0]
    c11 = texture[y1, x1]
    top = c00 * (1.0 - fx[..., None]) + c10 * fx[..., None]
    bottom = c01 * (1.0 - fx[..., None]) + c11 * fx[..., None]
    return top * (1.0 - fy[..., None]) + bottom * fy[..., None]
