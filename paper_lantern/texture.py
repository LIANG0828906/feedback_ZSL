from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .util import atomic_write_bytes, sha256_bytes

_C1 = np.uint64(0x9E3779B97F4A7C15)
_C2 = np.uint64(0xBF58476D1CE4E5B9)
_C3 = np.uint64(0x94D049BB133111EB)


@dataclass(frozen=True)
class Texture:
    layer_index: int
    resolution: int
    table: np.ndarray

    @property
    def paper_mask(self) -> np.ndarray:
        return self.table[:, :, 0]

    @property
    def transmittance_modulation(self) -> np.ndarray:
        return self.table[:, :, 1]

    @property
    def scatter_modulation(self) -> np.ndarray:
        return self.table[:, :, 2]

    def raw_bytes(self) -> bytes:
        return np.ascontiguousarray(self.table, dtype="<f4").tobytes(order="C")

    @property
    def digest(self) -> str:
        return sha256_bytes(self.raw_bytes())


def _splitmix64(value: np.ndarray | np.uint64) -> np.ndarray:
    with np.errstate(over="ignore"):
        value = np.asarray(value, dtype=np.uint64)
        value = value + _C1
        value = (value ^ (value >> np.uint64(30))) * _C2
        value = (value ^ (value >> np.uint64(27))) * _C3
        return value ^ (value >> np.uint64(31))


def _seed_for(seed: int, layer_index: int, resolution: int, stream: int) -> np.uint64:
    base = np.uint64(seed & 0xFFFFFFFFFFFFFFFF)
    base ^= np.uint64((layer_index + 1) * 0xD1B54A32D192ED03 & 0xFFFFFFFFFFFFFFFF)
    base ^= np.uint64((resolution + 17) * 0x94D049BB133111EB & 0xFFFFFFFFFFFFFFFF)
    base ^= np.uint64((stream + 1) * 0x9E3779B97F4A7C15 & 0xFFFFFFFFFFFFFFFF)
    return _splitmix64(base)


def _value_noise(resolution: int, frequency: int, seed: np.uint64) -> np.ndarray:
    coordinate = (np.arange(resolution, dtype=np.float64) + 0.5) / resolution
    lattice = coordinate * frequency
    base = np.floor(lattice).astype(np.int64)
    fraction = lattice - base
    smooth = fraction * fraction * (3.0 - 2.0 * fraction)
    values = np.empty((frequency + 1, frequency + 1), dtype=np.float64)
    for y in range(frequency + 1):
        for x in range(frequency + 1):
            hashed = _splitmix64(
                seed
                ^ np.uint64(x * 0x9E3779B1)
                ^ np.uint64(y * 0x85EBCA77)
            )
            values[y, x] = float(
                np.right_shift(hashed, np.uint64(11)) & np.uint64((1 << 53) - 1)
            ) / float(1 << 53)
    x0 = base % frequency
    y0 = base % frequency
    x1 = (x0 + 1) % frequency
    y1 = (y0 + 1) % frequency
    sx = smooth[None, :]
    sy = smooth[:, None]
    top = values[np.ix_(y0, x0)] * (1.0 - sx) + values[np.ix_(y0, x1)] * sx
    bottom = values[np.ix_(y1, x0)] * (1.0 - sx) + values[np.ix_(y1, x1)] * sx
    return top * (1.0 - sy) + bottom * sy


def _flower_mask(u: np.ndarray, v: np.ndarray, layer_index: int, ragged: np.ndarray) -> np.ndarray:
    repeat_x = 3 + (layer_index % 3)
    repeat_y = 2 + ((layer_index + 1) % 2)
    cx = np.mod(u * repeat_x, 1.0) - 0.5
    cy = np.mod(v * repeat_y, 1.0) - 0.5
    edge_scale = 1.0 + (ragged - 0.5) * 0.18

    center = np.hypot(cx, cy) < 0.105 * edge_scale
    petals = (
        (np.hypot(np.abs(cx) - 0.185, cy) < 0.095 * edge_scale)
        | (np.hypot(cx, np.abs(cy) - 0.185) < 0.095 * edge_scale)
    )
    diamond = (np.abs(cx) + np.abs(cy)) < 0.095 * edge_scale
    hole = center | petals | diamond
    border = (u < 0.018) | (u > 0.982) | (v < 0.025) | (v > 0.975)
    return (~hole | border).astype(np.float64)


def generate_texture(seed: int, layer_index: int, resolution: int) -> Texture:
    if resolution < 4:
        raise ValueError("resolution must be at least 4")
    coordinate = (np.arange(resolution, dtype=np.float64) + 0.5) / resolution
    u, v = np.meshgrid(coordinate, coordinate)

    seed_noise_a = _seed_for(seed, layer_index, resolution, 1)
    noise_a = _value_noise(resolution, 8, seed_noise_a)
    noise_b = _value_noise(resolution, 23, _seed_for(seed, layer_index, resolution, 2))
    noise_c = _value_noise(resolution, 47, _seed_for(seed, layer_index, resolution, 3))

    angle_seed = _seed_for(seed, layer_index, resolution, 4)
    angle_bits = int(angle_seed & np.uint64(0xFFFFFFFF))
    angle = 0.35 + (angle_bits / float(0xFFFFFFFF)) * 2.3
    direction = np.cos(angle) * u + np.sin(angle) * v
    phase_bits = int(
        _seed_for(seed, layer_index, resolution, 5) & np.uint64(0xFFFFFFFF)
    )
    phase = phase_bits / float(0xFFFFFFFF) * 2.0 * np.pi
    fibers = 0.5 + 0.5 * np.sin(2.0 * np.pi * (34.0 * direction) + phase)
    fine = 0.5 + 0.5 * np.sin(
        2.0 * np.pi * (91.0 * (np.cos(angle + 1.2) * u + np.sin(angle + 1.2) * v))
        + phase * 0.37
    )
    fiber = np.clip(
        0.40 * noise_a + 0.20 * noise_b + 0.12 * noise_c + 0.18 * fibers + 0.10 * fine,
        0.0,
        1.0,
    )
    ragged = np.clip(0.55 * noise_b + 0.45 * noise_c, 0.0, 1.0)
    paper_mask = _flower_mask(u, v, layer_index, ragged)

    transmittance_modulation = 0.73 + 0.34 * fiber
    scatter_modulation = 0.68 + 0.48 * np.clip(0.65 * fiber + 0.35 * noise_a, 0.0, 1.0)
    table = np.stack(
        (
            paper_mask,
            transmittance_modulation * paper_mask,
            scatter_modulation * paper_mask,
        ),
        axis=2,
    ).astype(np.float32)
    return Texture(layer_index=layer_index, resolution=resolution, table=table)


def sample_bilinear(table: np.ndarray, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    """Bilinear table sampling. Inputs may be any broadcast-compatible shape."""
    height, width = table.shape[:2]
    u = np.asarray(u, dtype=np.float64)
    v = np.asarray(v, dtype=np.float64)
    x = np.clip(u * width - 0.5, 0.0, width - 1.0)
    y = np.clip(v * height - 0.5, 0.0, height - 1.0)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, width - 1)
    y1 = np.minimum(y0 + 1, height - 1)
    wx = x - x0
    wy = y - y0
    top = table[y0, x0] * (1.0 - wx)[..., None] + table[y0, x1] * wx[..., None]
    bottom = table[y1, x0] * (1.0 - wx)[..., None] + table[y1, x1] * wx[..., None]
    return top * (1.0 - wy)[..., None] + bottom * wy[..., None]


def write_texture_bundle(
    output_dir: str | Path,
    seed: int,
    layers: list[tuple[int, int]],
) -> dict:
    texture_dir = Path(output_dir) / "textures"
    records: list[dict] = []
    combined = bytearray()
    for layer_index, resolution in sorted(layers):
        texture = generate_texture(seed, layer_index, resolution)
        filename = f"layer_{layer_index:02d}.f32"
        payload = texture.raw_bytes()
        atomic_write_bytes(texture_dir / filename, payload)
        combined.extend(payload)
        records.append(
            {
                "layer_index": layer_index,
                "resolution": resolution,
                "channels": ["paper_mask", "transmittance_modulation", "scatter_modulation"],
                "dtype": "<f4",
                "file": f"textures/{filename}",
                "sha256": texture.digest,
            }
        )
    manifest = {
        "format": "paper-lantern-textures-v1",
        "seed": seed,
        "textures": records,
        "bundle_sha256": sha256_bytes(bytes(combined)),
    }
    atomic_write_bytes(
        texture_dir / "manifest.json",
        (json.dumps(manifest, indent=2, sort_keys=False) + "\n").encode("utf-8"),
    )
    return manifest

