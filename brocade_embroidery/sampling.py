"""Deterministic embroidery and hole sampling tables.

Both tables are pure functions of an integer seed, layer index and resolution.
They are stored as little-endian float32 arrays so repeated generation is
byte-identical and independent of triangle traversal order.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .rng import hash_mix, uniform_grid


@dataclass(frozen=True)
class EmbroideryTable:
    layer_index: int
    seed: int
    resolution: int
    modulation: np.ndarray
    thread_angle: np.ndarray
    anisotropy_scale: np.ndarray

    def to_bytes(self) -> bytes:
        parts = [
            np.asarray(self.modulation, dtype="<f4").tobytes(),
            np.asarray(self.thread_angle, dtype="<f4").tobytes(),
            np.asarray(self.anisotropy_scale, dtype="<f4").tobytes(),
        ]
        return b"".join(parts)


@dataclass(frozen=True)
class HoleTable:
    layer_index: int
    seed: int
    resolution: int
    coverage: np.ndarray

    def to_bytes(self) -> bytes:
        return np.asarray(self.coverage, dtype="<f4").tobytes()


def _smoothstep(values: np.ndarray) -> np.ndarray:
    return values * values * (3.0 - 2.0 * values)


def value_noise(seed: int, lane: int, resolution: int,
                cell_count: int) -> np.ndarray:
    """Bilinearly interpolated deterministic value noise on a wrapped grid."""
    lattice = uniform_grid(seed, lane, cell_count)
    axis = (np.arange(resolution, dtype=np.float32) + 0.5) / resolution
    grid_x, grid_y = np.meshgrid(axis, axis, indexing="xy")
    fx = grid_x * cell_count - 0.5
    fy = grid_y * cell_count - 0.5
    x0 = np.floor(fx).astype(np.int64) % cell_count
    y0 = np.floor(fy).astype(np.int64) % cell_count
    x1 = (x0 + 1) % cell_count
    y1 = (y0 + 1) % cell_count
    tx = _smoothstep((fx - np.floor(fx)).astype(np.float32))
    ty = _smoothstep((fy - np.floor(fy)).astype(np.float32))
    top = lattice[y0, x0] * (1.0 - tx) + lattice[y0, x1] * tx
    bottom = lattice[y1, x0] * (1.0 - tx) + lattice[y1, x1] * tx
    return top * (1.0 - ty) + bottom * ty


def build_embroidery_table(seed: int, layer_index: int,
                           resolution: int) -> EmbroideryTable:
    """Create deterministic stitch modulation, thread angle and anisotropy.

    Low-frequency value noise gives flowing embroidered motifs while a second
    hash grid adds fine stitch variation.  Every value derives from
    ``hash_mix(seed, layer, resolution, row, col)``.
    """
    # Three smooth octaves replace nearest-neighbour blocks; every octave is a
    # pure hash-grid function of seed/layer/resolution and stays deterministic.
    coarse_full = value_noise(seed, layer_index * 10 + 1,
                              resolution, max(8, resolution // 16))
    medium = value_noise(seed, layer_index * 10 + 5,
                         resolution, max(12, resolution // 8))
    fine = value_noise(seed, layer_index * 10 + 2,
                       resolution, max(16, resolution // 4))
    thread_hash = uniform_grid(seed, layer_index * 10 + 3, resolution)
    anisotropy_hash = uniform_grid(seed, layer_index * 10 + 4, resolution)

    modulation = (0.55 * coarse_full
                  + 0.30 * medium
                  + 0.15 * fine).astype(np.float32)
    modulation = np.clip(modulation, 0.0, 1.0).astype(np.float32)
    thread_angle = (thread_hash * np.float32(0.9)).astype(np.float32)
    anisotropy_scale = (np.float32(0.72)
                        + anisotropy_hash * np.float32(0.56)).astype(np.float32)
    return EmbroideryTable(
        layer_index=layer_index,
        seed=seed,
        resolution=resolution,
        modulation=modulation,
        thread_angle=thread_angle,
        anisotropy_scale=anisotropy_scale,
    )


def _hole_cell_centers(seed: int, layer_index: int, grid_side: int,
                       hole_rate: float) -> tuple[np.ndarray, np.ndarray,
                                                  np.ndarray]:
    """Return fractional hole centers and radii on a jittered regular grid."""
    rows = []
    cols = []
    radii = []
    for row in range(grid_side):
        for col in range(grid_side):
            selection = hash_mix(seed, layer_index, grid_side, row, col, 77)
            if (selection % np.uint64(1000)) >= np.uint64(round(hole_rate * 1000)):
                continue
            jitter_r = uniform_grid(seed, layer_index * 13 + 5,
                                    grid_side)[row, col]
            jitter_c = uniform_grid(seed, layer_index * 13 + 6,
                                    grid_side)[row, col]
            radius_hash = uniform_grid(seed, layer_index * 13 + 7,
                                       grid_side)[row, col]
            rows.append((row + 0.18 + 0.64 * jitter_r) / grid_side)
            cols.append((col + 0.18 + 0.64 * jitter_c) / grid_side)
            radii.append(0.045 + 0.05 * radius_hash)
    return (np.asarray(rows, dtype=np.float32),
            np.asarray(cols, dtype=np.float32),
            np.asarray(radii, dtype=np.float32))


def build_hole_table(seed: int, layer_index: int, resolution: int,
                     hole_rate: float) -> HoleTable:
    """Build a soft coverage field.

    ``coverage`` is 1 on solid Kesi silk and approaches 0 inside holes.  Edges
    are already soft at table granularity; shadow projection widens them into
    the physical penumbra.  The grid side is fixed from the seed-independent
    resolution so layout changes only when the seed or rate changes.
    """
    grid_side = max(6, resolution // 16)
    rows, cols, radii = _hole_cell_centers(seed, layer_index, grid_side,
                                           hole_rate)
    axis = (np.arange(resolution, dtype=np.float32) + 0.5) / resolution
    px, py = np.meshgrid(axis, axis, indexing="xy")
    coverage = np.ones((resolution, resolution), dtype=np.float32)
    for center_r, center_c, radius in zip(rows, cols, radii):
        distance = np.sqrt((px - center_c) ** 2 + (py - center_r) ** 2)
        # Soft two-pixel-wide edge inside the table, independent of receiver.
        edge = np.float32(max(1.5 / resolution, radius * 0.18))
        hole = np.clip((radius + edge - distance) / edge, 0.0, 1.0)
        coverage = np.minimum(coverage, np.float32(1.0) - hole.astype(np.float32))
    return HoleTable(layer_index=layer_index, seed=seed,
                     resolution=resolution, coverage=coverage.astype(np.float32))


def bilinear_sample(table: np.ndarray, u: np.ndarray,
                    v: np.ndarray) -> np.ndarray:
    """Bilinear sample with wrap addressing; u/v may be any shape."""
    u = np.asarray(u, dtype=np.float64)
    v = np.asarray(v, dtype=np.float64)
    resolution = table.shape[0]
    u_wrapped = np.mod(u, 1.0)
    v_wrapped = np.mod(v, 1.0)
    fx = u_wrapped * resolution - 0.5
    fy = v_wrapped * resolution - 0.5
    x0 = np.floor(fx).astype(np.int64)
    y0 = np.floor(fy).astype(np.int64)
    tx = (fx - x0).astype(np.float32)
    ty = (fy - y0).astype(np.float32)
    x1 = (x0 + 1) % resolution
    y1 = (y0 + 1) % resolution
    x0 %= resolution
    y0 %= resolution
    top = table[y0, x0] * (1.0 - tx) + table[y0, x1] * tx
    bottom = table[y1, x0] * (1.0 - tx) + table[y1, x1] * tx
    return top * (1.0 - ty) + bottom * ty
