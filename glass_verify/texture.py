from __future__ import annotations

import hashlib
from pathlib import Path

import numpy as np

from .scene import Scene, TextureConfig

MASK64 = (1 << 64) - 1


def _splitmix64(value: int) -> int:
    value = (value + 0x9E3779B97F4A7C15) & MASK64
    value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & MASK64
    return (value ^ (value >> 31)) & MASK64


def _hash_sequence(*values: int) -> int:
    state = 0xCBF29CE484222325
    for value in values:
        state = _splitmix64(state ^ (int(value) & MASK64))
    return state


def _unit_float(value: int) -> float:
    return ((value >> 11) & ((1 << 53) - 1)) / float((1 << 53) - 1)


def _smoothstep(value: float) -> float:
    return value * value * (3.0 - 2.0 * value)


def _value_noise(seed: int, frequency: int, x: float, y: float) -> float:
    fx = x * frequency
    fy = y * frequency
    ix = int(np.floor(fx))
    iy = int(np.floor(fy))
    tx = _smoothstep(fx - ix)
    ty = _smoothstep(fy - iy)
    grid = []
    for yy in (iy, iy + 1):
        row = []
        for xx in (ix, ix + 1):
            row.append(_unit_float(_hash_sequence(seed, xx & 0xFFFFFFFF, yy & 0xFFFFFFFF, frequency)))
        grid.append(row)
    top = grid[0][0] * (1.0 - tx) + grid[0][1] * tx
    bottom = grid[1][0] * (1.0 - tx) + grid[1][1] * tx
    return top * (1.0 - ty) + bottom * ty


def pane_texture_seed(scene_seed: int, sector_id: int, pane_id: int) -> int:
    return _hash_sequence(scene_seed, 0x51A55, sector_id, pane_id)


def pane_variation(scene_seed: int, sector_id: int, pane_id: int) -> float:
    """A stable per-pane gain in [0.92,1.08], independent of list ordering."""
    value = _unit_float(_hash_sequence(scene_seed, 0xB17E, sector_id, pane_id))
    return 0.92 + 0.16 * value


def generate_texture_table(config: TextureConfig, sector_id: int, pane_id: int) -> np.ndarray:
    seed = pane_texture_seed(config.seed, sector_id, pane_id)
    size = config.table_size
    xs = (np.arange(size, dtype=np.float64) + 0.5) / size
    ys = (np.arange(size, dtype=np.float64) + 0.5) / size
    table = np.zeros((size, size), dtype=np.float64)
    total_weight = 0.0
    for octave in range(config.octaves):
        frequency = 2 ** (octave + 1)
        weight = 1.0 / (1.5 ** octave)
        total_weight += weight
        for y in range(size):
            for x in range(size):
                table[y, x] += weight * _value_noise(
                    seed ^ (octave * 0xD1B54A32D192ED03), frequency, xs[x], ys[y]
                )
    table /= total_weight
    # Per-pane phase tilt guarantees that equal-color neighbours are not identical.
    phase = _unit_float(_hash_sequence(seed, 0xA11CE))
    table = table * 0.80 + 0.10 + 0.08 * ((xs[None, :] + ys[:, None] + phase) % 1.0)
    return np.clip(table, 0.0, 1.0).astype(np.float32)


def build_texture_tables(scene: Scene) -> np.ndarray:
    tables = [
        generate_texture_table(scene.texture, pane.sector_id, pane.pane_id)
        for pane in scene.panes
    ]
    return np.stack(tables, axis=0)


def texture_digest(tables: np.ndarray) -> str:
    array = np.ascontiguousarray(tables, dtype="<f4")
    return "sha256:" + hashlib.sha256(array.tobytes(order="C")).hexdigest()


def save_texture_tables(path: Path, tables: np.ndarray, scene: Scene) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    import io

    buffer = io.BytesIO()
    np.save(buffer, tables, allow_pickle=False)
    from .pngio import atomic_write_bytes
    atomic_write_bytes(path, buffer.getvalue())


def sample_texture(
    table: np.ndarray,
    uv: np.ndarray,
) -> np.ndarray:
    """Bilinear, edge-clamped texture sample for an (...,2) UV array."""
    size = table.shape[0]
    u = np.clip(np.asarray(uv)[..., 0], 0.0, 1.0) * size - 0.5
    v = np.clip(np.asarray(uv)[..., 1], 0.0, 1.0) * size - 0.5
    x0 = np.floor(u).astype(np.int64)
    y0 = np.floor(v).astype(np.int64)
    fx = (u - x0).astype(np.float32)
    fy = (v - y0).astype(np.float32)
    x1 = np.clip(x0 + 1, 0, size - 1)
    y1 = np.clip(y0 + 1, 0, size - 1)
    x0 = np.clip(x0, 0, size - 1)
    y0 = np.clip(y0, 0, size - 1)
    top = table[y0, x0] * (1.0 - fx) + table[y0, x1] * fx
    bottom = table[y1, x0] * (1.0 - fx) + table[y1, x1] * fx
    return top * (1.0 - fy) + bottom * fy
