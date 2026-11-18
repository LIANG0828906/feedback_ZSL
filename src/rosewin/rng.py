"""Deterministic integer hashing, PRNG and per-facet fibre texture tables.

No global random state is used anywhere in the renderer. Every random draw is
derived from the scene seed, the stable facet id and a fixed lane index, so the
output is independent of facet traversal order and identical byte-for-byte
across runs.
"""

from __future__ import annotations

import numpy as np

U64_MASK = np.uint64(0xFFFFFFFFFFFFFFFF)


def splitmix64(state: np.uint64 | int) -> tuple[np.uint64, np.uint64]:
    """One deterministic splitmix64 step, returning ``(next_state, value)``."""
    with np.errstate(over="ignore"):
        state = (np.uint64(state) + np.uint64(0x9E3779B97F4A7C15)) & U64_MASK
        z = state
        z = ((z ^ (z >> np.uint64(30))) * np.uint64(0xBF58476D1CE4E5B9)) & U64_MASK
        z = ((z ^ (z >> np.uint64(27))) * np.uint64(0x94D049BB133111EB)) & U64_MASK
        out = z ^ (z >> np.uint64(31))
    return state, out


def _hash_mix(a: int, b: int, c: int = 0) -> np.uint64:
    """Fold three integer lanes into a single 64 bit hash."""
    h = np.uint64(0xCBF29CE484222325)
    with np.errstate(over="ignore"):
        for value in (a, b, c):
            h = (h ^ np.uint64(value & 0xFFFFFFFFFFFFFFFF)) & U64_MASK
            h = (h * np.uint64(0x100000001B3)) & U64_MASK
    return h


class DeterministicRng:
    """Stream of deterministic floats backed by splitmix64 only."""

    def __init__(self, seed: int, stream: int):
        self._state = int(_hash_mix(seed, stream))

    def uniform(self, count: int) -> np.ndarray:
        out = np.empty(count, dtype=np.float64)
        state = self._state
        for index in range(count):
            state, value = splitmix64(state)
            out[index] = (value.astype(np.float64) + 0.5) / float(1 << 64)
        self._state = state
        return out


def _smoothstep(t: np.ndarray) -> np.ndarray:
    return t * t * (3.0 - 2.0 * t)


def fibre_texture_table(
    seed: int,
    stable_id: int,
    table_size: int,
    scale: float,
) -> np.ndarray:
    """Build a ``table_size x table_size`` mottling table for one facet.

    The table stores floats in ``[0.75, 1.0]`` (glass never gains energy from
    its own texture), interpolated with smoothstep value noise over a seeded
    integer lattice plus one finer octave. Sampling is keyed by stable facet
    id rather than list position.
    """

    rng = DeterministicRng(seed, 0x67_6C_61_73 ^ stable_id)
    lattice_n = table_size + 2
    coarse = rng.uniform(lattice_n * lattice_n).reshape(lattice_n, lattice_n)
    fine = rng.uniform(lattice_n * lattice_n).reshape(lattice_n, lattice_n)

    axis = np.arange(table_size, dtype=np.float64)
    gx, gy = np.meshgrid(axis, axis, indexing="xy")

    def sample_layer(grid: np.ndarray, layer_scale: float) -> np.ndarray:
        pos = (gx * layer_scale / table_size) * (lattice_n - 1)
        pos_y = (gy * layer_scale / table_size) * (lattice_n - 1)
        ix = np.floor(pos).astype(np.int64)
        iy = np.floor(pos_y).astype(np.int64)
        ix = np.clip(ix, 0, lattice_n - 2)
        iy = np.clip(iy, 0, lattice_n - 2)
        tx = _smoothstep(pos - ix)
        ty = _smoothstep(pos_y - iy)
        v00 = grid[iy, ix]
        v10 = grid[iy, ix + 1]
        v01 = grid[iy + 1, ix]
        v11 = grid[iy + 1, ix + 1]
        top = v00 + (v10 - v00) * tx
        bottom = v01 + (v11 - v01) * tx
        return top + (bottom - top) * ty

    layer0 = sample_layer(coarse, scale)
    layer1 = sample_layer(fine, scale * 3.0)
    mixed = 0.8 * layer0 + 0.2 * layer1
    normalized = (mixed - mixed.min()) / (mixed.ptp() if mixed.ptp() > 0.0 else 1.0)
    return (0.75 + 0.25 * normalized).astype(np.float64)


def sample_table(table: np.ndarray, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    """Bilinear periodic table sampling at normalized ``u, v`` coordinates."""
    size = table.shape[0]
    px = np.mod(u, 1.0) * size
    py = np.mod(v, 1.0) * size
    ix = np.floor(px).astype(np.int64) % size
    iy = np.floor(py).astype(np.int64) % size
    ix1 = (ix + 1) % size
    iy1 = (iy + 1) % size
    tx = px - np.floor(px)
    ty = py - np.floor(py)
    top = table[iy, ix] * (1.0 - tx) + table[iy, ix1] * tx
    bottom = table[iy1, ix] * (1.0 - tx) + table[iy1, ix1] * tx
    return top * (1.0 - ty) + bottom * ty
