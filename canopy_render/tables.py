from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache

import numpy as np

from .util import seeded_uniform


TABLE_SIZE = 64


def _smooth_table(values: np.ndarray, passes: int = 3) -> np.ndarray:
    table = values.reshape(TABLE_SIZE, TABLE_SIZE).copy()
    for _ in range(passes):
        table = (
            table
            + np.roll(table, 1, axis=0)
            + np.roll(table, -1, axis=0)
            + np.roll(table, 1, axis=1)
            + np.roll(table, -1, axis=1)
        ) / 5.0
    lo = float(table.min())
    hi = float(table.max())
    if hi <= lo:
        return np.zeros_like(table)
    return (table - lo) / (hi - lo) * 2.0 - 1.0


@dataclass(frozen=True)
class MaterialTables:
    pattern: np.ndarray
    normal_u: np.ndarray
    normal_v: np.ndarray
    seed: int


@lru_cache(maxsize=64)
def material_tables(seed: int) -> MaterialTables:
    """Build all material tables from the layer seed alone.

    No global random generator is consulted, so changing layer traversal order
    cannot alter these tables.
    """
    seed = int(seed)
    size2 = TABLE_SIZE * TABLE_SIZE
    pattern = (_smooth_table(seeded_uniform(size2, seed, 0x11), 3) + 1.0) * 0.5
    normal_u = _smooth_table(seeded_uniform(size2, seed, 0x22), 2)
    normal_v = _smooth_table(seeded_uniform(size2, seed, 0x33), 2)
    pattern.setflags(write=False)
    normal_u.setflags(write=False)
    normal_v.setflags(write=False)
    return MaterialTables(
        pattern=pattern,
        normal_u=normal_u,
        normal_v=normal_v,
        seed=seed,
    )


def _bilinear_wrapped(table: np.ndarray, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    x = (u - np.floor(u)) * TABLE_SIZE
    y = (v - np.floor(v)) * TABLE_SIZE
    x0 = np.floor(x).astype(np.int64) % TABLE_SIZE
    y0 = np.floor(y).astype(np.int64) % TABLE_SIZE
    x1 = (x0 + 1) % TABLE_SIZE
    y1 = (y0 + 1) % TABLE_SIZE
    fx = x - np.floor(x)
    fy = y - np.floor(y)
    return (
        table[y0, x0] * (1.0 - fx) * (1.0 - fy)
        + table[y0, x1] * fx * (1.0 - fy)
        + table[y1, x0] * (1.0 - fx) * fy
        + table[y1, x1] * fx * fy
    )


def sample_tables(
    tables: MaterialTables,
    u: np.ndarray,
    v: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    pattern = _bilinear_wrapped(tables.pattern, u, v)
    du = _bilinear_wrapped(tables.normal_u, u, v)
    dv = _bilinear_wrapped(tables.normal_v, u, v)
    return pattern, du, dv


def brocade_pattern(
    table_value: np.ndarray,
    u: np.ndarray,
    v: np.ndarray,
    seed: int,
) -> np.ndarray:
    """Combine the seeded table with deterministic woven threads."""
    phase = (int(seed) % 6283) / 1000.0
    thread_a = np.sin(np.pi * 2.0 * (u * 18.0 + v * 7.0) + phase)
    thread_b = np.sin(np.pi * 2.0 * (u * 9.0 - v * 22.0) + phase * 0.71)
    diagonal = np.sin(np.pi * 2.0 * (u * 5.0 + v * 5.0) + phase * 0.37)
    weave = (
        0.53
        + 0.16 * thread_a
        + 0.14 * thread_b
        + 0.10 * diagonal
        + 0.07 * table_value
    )
    # Keep a non-zero floor so pattern remains legible in occluded folds.
    return np.clip(weave, 0.12, 0.94)

