"""Deterministic NumPy sampling helpers."""

from __future__ import annotations

import numpy as np


def sample_trilinear(volume: np.ndarray, coords: np.ndarray) -> np.ndarray:
    """Trilinearly sample a ``(z, y, x)`` array at ``(x, y, z)`` coordinates.

    Out-of-range coordinates are clamped to the nearest voxel center. This is
    deliberate: ray marching stays inside the AABB while central differences
    may reach one voxel beyond a boundary voxel.
    """

    volume = np.asarray(volume)
    if volume.ndim != 3:
        raise ValueError("volume must be three-dimensional")
    points = np.asarray(coords, dtype=np.float64)
    if points.shape[-1] != 3:
        raise ValueError("coordinates must end with an (x, y, z) triple")

    nz, ny, nx = volume.shape
    x = np.clip(points[..., 0], 0.0, nx - 1.0)
    y = np.clip(points[..., 1], 0.0, ny - 1.0)
    z = np.clip(points[..., 2], 0.0, nz - 1.0)

    x0 = np.floor(x).astype(np.intp)
    y0 = np.floor(y).astype(np.intp)
    z0 = np.floor(z).astype(np.intp)
    x1 = np.minimum(x0 + 1, nx - 1)
    y1 = np.minimum(y0 + 1, ny - 1)
    z1 = np.minimum(z0 + 1, nz - 1)

    fx = x - x0
    fy = y - y0
    fz = z - z0

    c000 = volume[z0, y0, x0]
    c100 = volume[z0, y0, x1]
    c010 = volume[z0, y1, x0]
    c110 = volume[z0, y1, x1]
    c001 = volume[z1, y0, x0]
    c101 = volume[z1, y0, x1]
    c011 = volume[z1, y1, x0]
    c111 = volume[z1, y1, x1]

    c00 = c000 * (1.0 - fx) + c100 * fx
    c10 = c010 * (1.0 - fx) + c110 * fx
    c01 = c001 * (1.0 - fx) + c101 * fx
    c11 = c011 * (1.0 - fx) + c111 * fx
    c0 = c00 * (1.0 - fy) + c10 * fy
    c1 = c01 * (1.0 - fy) + c11 * fy
    return c0 * (1.0 - fz) + c1 * fz
