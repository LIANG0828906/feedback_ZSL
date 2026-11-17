"""Normal computation by central differencing, plus seam repair.

Two ways to derive per-chunk normals:

* ``chunk_normals_local`` uses only the chunk's own vertices (clamped
  differences at the chunk edge). This is what a naive independent per-chunk
  bake would produce and it disagrees with neighbours on shared edges.
* ``chunk_normals_with_halo`` samples a one-vertex ring of *neighbouring
  chunk* heights so boundary vertices use true central differences. This is
  identical to extracting the chunk from a globally computed normal field.

``repair_seams`` merges the two: interior vertices keep the local result,
boundary vertices are replaced by the halo result, and the number of boundary
vertices whose normal actually changed is reported.
"""

import numpy as np


def _normals_from_heights(h, step_x, step_z):
    """Unit normals for a height block via central differences.

    Edges of the given block fall back to forward/backward differences.
    """
    dhdx = np.empty_like(h)
    dhdz = np.empty_like(h)
    dhdx[:, 1:-1] = (h[:, 2:] - h[:, :-2]) / (2.0 * step_x)
    dhdx[:, 0] = (h[:, 1] - h[:, 0]) / step_x
    dhdx[:, -1] = (h[:, -1] - h[:, -2]) / step_x
    dhdz[1:-1, :] = (h[2:, :] - h[:-2, :]) / (2.0 * step_z)
    dhdz[0, :] = (h[1, :] - h[0, :]) / step_z
    dhdz[-1, :] = (h[-1, :] - h[-2, :]) / step_z
    normals = np.stack([-dhdx, np.ones_like(h), -dhdz], axis=-1)
    lengths = np.linalg.norm(normals, axis=-1)
    if np.any(lengths == 0.0) or np.any(~np.isfinite(lengths)):
        raise ArithmeticError("degenerate normal encountered")
    return normals / lengths[..., None]


def global_normals(heights, step_x, step_z):
    """One-shot normals over the full global heightfield."""
    return _normals_from_heights(heights, step_x, step_z)


def chunk_normals_local(chunk_heights, step_x, step_z):
    """Normals computed from the chunk's own vertices only."""
    return _normals_from_heights(chunk_heights, step_x, step_z)


def extract_halo(heights, ox, oy, size):
    """Chunk heights plus a one-vertex halo, clamped at the world border.

    Returns (halo, off_y, off_x) where the slice
    halo[off_y:off_y+size, off_x:off_x+size] is the chunk itself.
    """
    rows, cols = heights.shape
    y0, y1 = max(oy - 1, 0), min(oy + size + 1, rows)
    x0, x1 = max(ox - 1, 0), min(ox + size + 1, cols)
    return heights[y0:y1, x0:x1], oy - y0, ox - x0


def chunk_normals_with_halo(heights, ox, oy, size, step_x, step_z):
    """Boundary-correct chunk normals sampling neighbouring chunk heights."""
    halo, off_y, off_x = extract_halo(heights, ox, oy, size)
    halo_normals = _normals_from_heights(halo, step_x, step_z)
    return halo_normals[off_y:off_y + size, off_x:off_x + size]


def repair_seams(local_normals, halo_normals):
    """Merge boundary normals; returns (repaired, repaired_vertex_count).

    Interior vertices are kept from the local computation; every vertex on
    the chunk's outer ring takes the halo (neighbour-aware) normal so that
    shared edges are component-wise identical across chunks.
    """
    repaired = local_normals.copy()
    n = local_normals.shape[0]
    ring = np.zeros((n, n), dtype=bool)
    ring[0, :] = ring[-1, :] = True
    ring[:, 0] = ring[:, -1] = True
    changed = np.any(local_normals[ring] != halo_normals[ring], axis=-1)
    repaired[ring] = halo_normals[ring]
    return repaired, int(np.count_nonzero(changed))
