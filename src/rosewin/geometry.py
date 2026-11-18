"""Ray/facet intersection, lead distance and shading primitives."""

from __future__ import annotations

import numpy as np

from .model import Facet
from .rng import fibre_texture_table, sample_table


def intersect_facet_rays(
    origins: np.ndarray, directions: np.ndarray, facet: Facet
) -> tuple[np.ndarray, np.ndarray]:
    """Vectorized Moller-Trumbore intersection of all rays with one facet.

    Returns ``(t, hit)`` where ``t`` holds the nearest positive hit distance
    per ray and ``hit`` a boolean mask. All facets lie in the z=0 plane, so
    the hit point is ``origins + t * directions``.
    """

    ray_count = origins.shape[0]
    best_t = np.full(ray_count, np.inf, dtype=np.float64)
    vertices = facet.vertices
    for triangle in facet.triangles:
        v0 = vertices[triangle[0]]
        edge1 = vertices[triangle[1]] - v0
        edge2 = vertices[triangle[2]] - v0
        pvec = np.cross(directions, edge2)
        determinant = np.dot(pvec, edge1)
        parallel = np.abs(determinant) < 1e-12
        inverse = np.where(parallel, 1.0, 1.0 / np.where(parallel, 1.0, determinant))
        tvec = origins - v0
        u = np.sum(tvec * pvec, axis=1) * inverse
        qvec = np.cross(tvec, edge1)
        v = np.sum(directions * qvec, axis=1) * inverse
        t = np.sum(edge2 * qvec, axis=1) * inverse
        valid = (~parallel) & (u >= 0.0) & (v >= 0.0) & (u + v <= 1.0) & (t > 1e-9)
        update = valid & (t < best_t)
        best_t = np.where(update, t, best_t)
    return best_t, np.isfinite(best_t)


def lead_distance(points: np.ndarray, facet: Facet) -> np.ndarray:
    """Minimum planar distance from hit points to the facet boundary edges.

    A fan facet has two radial edges (at its start/end angle) and two arcs
    (inner and outer radius). The lead strip straddles each boundary, so a
    point is covered by lead when this distance is below half the width.
    """

    x = points[:, 0]
    y = points[:, 1]
    radius = np.hypot(x, y)
    distance = np.minimum(np.abs(radius - 0.24), np.abs(radius - 1.97))
    start_angle = float(np.arctan2(facet.vertices[0, 1], facet.vertices[0, 0]))
    end_angle = float(np.arctan2(facet.vertices[2, 1], facet.vertices[2, 0]))
    point_angle = np.arctan2(y, x)
    for edge_angle in (start_angle, end_angle):
        distance = np.minimum(distance, np.abs(radius * np.sin(point_angle - edge_angle)))
    return distance


class FacetMaterial:
    """Cached per-facet deterministic fibre texture and shading constants."""

    def __init__(self, facet: Facet, seed: int, table_size: int, scale: float):
        self.facet = facet
        self.table = fibre_texture_table(seed, facet.stable_id, table_size, scale)

    def mottle(self, points: np.ndarray) -> np.ndarray:
        radius = np.hypot(points[:, 0], points[:, 1])
        angle = np.arctan2(points[:, 1], points[:, 0])
        u = angle / (2.0 * np.pi) + 0.5
        v = (radius - 0.24) / (1.97 - 0.24)
        return sample_table(self.table, u, v)


def fresnel_schlick(cos_theta: np.ndarray, r0: np.ndarray) -> np.ndarray:
    """Per-channel Schlick reflection; cosine is clipped to a safe range."""
    cos_theta = np.clip(cos_theta, 0.0, 1.0)
    if cos_theta.ndim == 1:
        cos_theta = cos_theta[:, None]
    return r0 + (1.0 - r0) * (1.0 - cos_theta) ** 5
