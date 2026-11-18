"""Deterministic fiber texture and cutout mask generation.

Both the sample generator (which writes the scene JSON) and the renderer
(which validates the stored mesh) call the exact same functions, so the
stored cutout triangles can be re-derived byte-for-byte from integer seeds.
Nothing here uses global random state or depends on iteration order.
"""

from __future__ import annotations

import numpy as np

from .rng import PCG32, value_noise_3d


def cutout_triangle_mask(nx: int, ny: int, seed: int,
                         blob_count: int = 3,
                         hole_fraction: float = 0.18,
                         tunnel_cells: list | None = None) -> np.ndarray:
    """Build an irregular connected cutout pattern over a grid mesh.

    Holes grow as random-walk blobs across grid *cells*; both triangles of a
    pierced cell are marked, which keeps cutout edges exactly aligned with the
    mesh edges (no sub-triangle cracks).  ``tunnel_cells`` optionally lists
    cell indices forced open on every layer so a light path stays fully open
    through the whole stack.  The returned boolean array has one entry per
    triangle (``True`` means cut out) and length ``2 * nx * ny``.
    """

    nx, ny = int(nx), int(ny)
    rng = PCG32(int(seed))
    total_cells = nx * ny
    target = max(1, int(round(total_cells * float(hole_fraction))))
    pierced = np.zeros(total_cells, dtype=bool)

    blobs = max(1, int(blob_count))
    for _ in range(blobs):
        if pierced.sum() >= target:
            break
        cx = rng.randrange(0, nx)
        cy = rng.randrange(0, ny)
        walk_len = max(2, target // blobs + 2)
        for _ in range(walk_len):
            cell = cy * nx + cx
            if not pierced[cell]:
                pierced[cell] = True
            if pierced.sum() >= target:
                break
            direction = rng.randrange(0, 4)
            if direction == 0:
                cx = max(0, cx - 1)
            elif direction == 1:
                cx = min(nx - 1, cx + 1)
            elif direction == 2:
                cy = max(0, cy - 1)
            else:
                cy = min(ny - 1, cy + 1)

    if tunnel_cells:
        for cell in tunnel_cells:
            if 0 <= int(cell) < total_cells:
                pierced[int(cell)] = True

    triangle_mask = np.repeat(pierced, 2)
    return triangle_mask


def count_cutouts(mask: np.ndarray, nx: int, ny: int) -> int:
    """Count connected cutout blobs with 4-connectivity over grid cells."""

    mask = np.asarray(mask, dtype=bool)
    # Both triangles of a pierced cell share the same cutout state.
    cells = mask.reshape(-1, 2).any(axis=1)
    seen = np.zeros_like(cells)
    blobs = 0
    for start in np.flatnonzero(cells & ~seen):
        if seen[start]:
            continue
        blobs += 1
        stack = [int(start)]
        seen[start] = True
        while stack:
            cell = stack.pop()
            cx, cy = cell % nx, cell // nx
            neighbors = []
            if cx > 0:
                neighbors.append(cell - 1)
            if cx < nx - 1:
                neighbors.append(cell + 1)
            if cy > 0:
                neighbors.append(cell - nx)
            if cy < ny - 1:
                neighbors.append(cell + nx)
            for neighbor in neighbors:
                if cells[neighbor] and not seen[neighbor]:
                    seen[neighbor] = True
                    stack.append(neighbor)
    return blobs


def fiber_field(points: np.ndarray, seed: int) -> dict:
    """Fiber mottling sampled at arbitrary world-space paper points.

    Returns:
      transmittance_scale: multiplier around 1.0 for paper transmittance
      color_tint: per-channel multiplier around 1.0 for the scatter color
      mottle: the underlying 0..1 noise, useful for statistics

    Two value-noise octaves give directional fiber-like mottling.  Because the
    noise is keyed on world coordinates plus the layer seed, same-colored
    layers with different seeds remain distinguishable, and repeated runs are
    bit-identical regardless of traversal order.
    """

    pts = np.asarray(points, dtype=np.float64)
    coarse = value_noise_3d(pts * np.array([2.2, 2.2, 1.0]), seed)
    fine = value_noise_3d(pts * np.array([7.5, 7.5, 1.0]), seed + 101)
    mottle = np.clip(0.7 * coarse + 0.3 * fine, 0.0, 1.0)

    trans_scale = 0.86 + 0.28 * mottle
    tint_r = 0.90 + 0.20 * value_noise_3d(pts * 3.0, seed + 211)
    tint_g = 0.90 + 0.20 * value_noise_3d(pts * 3.0, seed + 307)
    tint_b = 0.90 + 0.20 * value_noise_3d(pts * 3.0, seed + 401)
    color_tint = np.stack([tint_r, tint_g, tint_b], axis=-1)
    return {
        "transmittance_scale": trans_scale,
        "color_tint": color_tint,
        "mottle": mottle,
    }


def apply_fiber(base_transmittance: float, scatter_color: np.ndarray,
                points: np.ndarray, seed: int) -> tuple[np.ndarray, np.ndarray]:
    """Modulate nominal transmittance and scatter color by the fiber field."""

    field = fiber_field(points, seed)
    trans = np.clip(base_transmittance * field["transmittance_scale"], 0.0, 1.0)
    color = np.clip(np.asarray(scatter_color, dtype=np.float64)[None, :]
                    * field["color_tint"], 0.0, 1.0)
    return trans, color
