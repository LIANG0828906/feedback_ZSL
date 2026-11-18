"""Geometry primitives and deterministic grid-mesh acceleration.

The sample scenes use axis-aligned planar rectangle meshes subdivided into a
regular triangle grid.  The grid fast path gives O(1) per-ray hit tests which
keeps soft shadow sampling tractable in pure NumPy.  A generic vectorized
Moller-Trumbore path is still provided so any conforming triangle soup from
JSON can be validated and rasterized.
"""

from __future__ import annotations

import numpy as np


def triangle_area(a: np.ndarray, b: np.ndarray, c: np.ndarray) -> float:
    return float(0.5 * np.linalg.norm(np.cross(b - a, c - a)))


def ray_plane_t(origins: np.ndarray, dirs: np.ndarray,
                plane_z: float) -> np.ndarray:
    """Signed parametric distance to the z = plane_z plane per ray."""

    denom = dirs[..., 2]
    t = np.full(origins.shape[:-1], np.inf, dtype=np.float64)
    valid = np.abs(denom) > 1e-12
    t[valid] = (plane_z - origins[..., 2][valid]) / denom[valid]
    return t


def evaluate_rays(origins: np.ndarray, dirs: np.ndarray, t: np.ndarray) -> np.ndarray:
    return origins + dirs * t[..., None]


def grid_ray_hit(origins: np.ndarray, dirs: np.ndarray, mesh: dict) -> dict:
    """Intersect rays with a planar grid mesh, honoring cutout triangles.

    Returns a dict of per-ray arrays:
      hit       - ray meets solid paper (grid cell present and not a hole)
      any_face  - ray meets the rectangle at all (hole or paper)
      tri_id    - triangle index when inside the rectangle, else -1
      point     - world-space hit position
      cell      - flattened grid cell index, else -1
    """

    meta = mesh["grid"]
    nx, ny = int(meta["nx"]), int(meta["ny"])
    xmin, xmax = float(meta["xmin"]), float(meta["xmax"])
    ymin, ymax = float(meta["ymin"]), float(meta["ymax"])
    plane_z = float(meta["z"])
    holes = np.asarray(mesh["holes_array"], dtype=bool)

    t = ray_plane_t(origins, dirs, plane_z)
    points = evaluate_rays(origins, dirs, t)
    x, y = points[..., 0], points[..., 1]

    fx = (x - xmin) / (xmax - xmin) * nx
    fy = (y - ymin) / (ymax - ymin) * ny
    ix = np.floor(fx).astype(np.int64)
    iy = np.floor(fy).astype(np.int64)
    in_bounds = (ix >= 0) & (ix < nx) & (iy >= 0) & (iy < ny) & (t > 0.0)

    cell = np.where(in_bounds, iy * nx + ix, -1)
    # Two triangles per cell; the split diagonal runs from (ix, iy) to
    # (ix+1, iy+1).  The upper triangle owns points above that diagonal.
    upper = (fx - np.floor(fx)) < (fy - np.floor(fy))
    tri_id = np.where(in_bounds, cell * 2 + np.where(upper, 1, 0), -1)
    solid = in_bounds & ~holes[np.clip(tri_id, 0, holes.size - 1)]

    return {
        "hit": solid,
        "any_face": in_bounds,
        "tri_id": tri_id,
        "point": points,
        "cell": cell,
    }


def ray_triangles(origins: np.ndarray, dirs: np.ndarray, verts: np.ndarray,
                  tris: np.ndarray, chunk: int = 4096) -> dict:
    """Generic Moller-Trumbore intersection for arbitrary triangle soups."""

    n = origins.shape[0]
    best_t = np.full(n, np.inf, dtype=np.float64)
    best_tri = np.full(n, -1, dtype=np.int64)
    eps = 1e-10

    for start in range(0, tris.shape[0], chunk):
        batch = tris[start:start + chunk]
        v0 = verts[batch[:, 0]]
        v1 = verts[batch[:, 1]]
        v2 = verts[batch[:, 2]]
        edge1 = v1 - v0
        edge2 = v2 - v0
        pvec = np.cross(dirs[:, None, :], edge2[None, :, :])
        det = np.sum(pvec * edge1[None, :, :], axis=2)
        valid = np.abs(det) > eps
        inv_det = np.where(valid, 1.0 / np.where(valid, det, 1.0), 0.0)
        tvec = origins[:, None, :] - v0[None, :, :]
        u = np.sum(tvec * pvec, axis=2) * inv_det
        qvec = np.cross(tvec, edge1[None, :, :])
        v = np.sum(dirs[:, None, :] * qvec, axis=2) * inv_det
        tt = np.sum(edge2[None, :, :] * qvec, axis=2) * inv_det
        ok = valid & (u >= 0.0) & (v >= 0.0) & ((u + v) <= 1.0) & (tt > eps)
        candidate = np.where(ok, tt, np.inf)
        local = np.argmin(candidate, axis=1)
        local_t = candidate[np.arange(n), local]
        improved = local_t < best_t
        best_t[improved] = local_t[improved]
        best_tri[improved] = start + local[improved]

    points = evaluate_rays(origins, dirs, best_t)
    return {"hit": best_tri >= 0, "tri_id": best_tri, "t": best_t,
            "point": points}


def perspective_project_z(point: np.ndarray, light: np.ndarray,
                          target_z: float) -> np.ndarray:
    """Project a point from the light onto the z=target_z paper plane."""

    point = np.asarray(point, dtype=np.float64)
    light = np.asarray(light, dtype=np.float64)
    delta = point - light
    if abs(delta[2]) < 1e-12:
        return point.copy()
    scale = (target_z - light[2]) / delta[2]
    return light + delta * scale


def point_in_grid_paper(point: np.ndarray, mesh: dict) -> bool:
    """True when ``point`` lies on solid (non-hole) paper of a grid mesh."""

    meta = mesh["grid"]
    nx, ny = int(meta["nx"]), int(meta["ny"])
    if abs(point[2] - float(meta["z"])) > 1e-9:
        return False
    fx = (point[0] - meta["xmin"]) / (meta["xmax"] - meta["xmin"]) * nx
    fy = (point[1] - meta["ymin"]) / (meta["ymax"] - meta["ymin"]) * ny
    ix, iy = int(np.floor(fx)), int(np.floor(fy))
    if not (0 <= ix < nx and 0 <= iy < ny):
        return False
    cell = iy * nx + ix
    upper = (fx - np.floor(fx)) < (fy - np.floor(fy))
    tri = cell * 2 + (1 if upper else 0)
    return not bool(np.asarray(mesh["holes_array"], dtype=bool)[tri])
