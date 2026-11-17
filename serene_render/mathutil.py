from __future__ import annotations

import numpy as np


EPS = 1.0e-12


def normalize(vector: np.ndarray) -> np.ndarray:
    arr = np.asarray(vector, dtype=np.float64)
    length = float(np.linalg.norm(arr))
    if length <= EPS:
        raise ValueError("cannot normalize a zero-length vector")
    return arr / length


def plane_basis(normal: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Return a deterministic orthonormal basis for a plane normal."""
    n = normalize(normal)
    reference = np.array([1.0, 0.0, 0.0], dtype=np.float64)
    if abs(float(n[0])) > 0.88:
        reference = np.array([0.0, 1.0, 0.0], dtype=np.float64)
    u = normalize(np.cross(reference, n))
    v = normalize(np.cross(n, u))
    return u, v


def newell_normal(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    normal = np.zeros(3, dtype=np.float64)
    for tri in triangles:
        a, b, c = vertices[int(tri[0])], vertices[int(tri[1])], vertices[int(tri[2])]
        normal += np.cross(a, b) + np.cross(b, c) + np.cross(c, a)
    return normalize(normal)


def canonical_vector(values: list[float] | tuple[float, ...]) -> np.ndarray:
    return np.asarray(values, dtype=np.float64)


def triangle_signed_distance_2d(
    points: np.ndarray,
    a: np.ndarray,
    b: np.ndarray,
    c: np.ndarray,
) -> np.ndarray:
    """Signed distance from 2D points to a triangle (negative inside)."""
    a = np.asarray(a, dtype=np.float64)
    b = np.asarray(b, dtype=np.float64)
    c = np.asarray(c, dtype=np.float64)

    area2 = float((b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0]))
    if area2 < 0.0:
        b, c = c, b
        area2 = -area2
    if area2 <= EPS:
        d_ab = _segment_distance(points, a, b)
        d_bc = _segment_distance(points, b, c)
        d_ca = _segment_distance(points, c, a)
        return np.minimum(np.minimum(d_ab, d_bc), d_ca)

    def edge_distance(p0: np.ndarray, p1: np.ndarray, opposite: np.ndarray) -> np.ndarray:
        edge = p1 - p0
        rel = points - p0
        cross = edge[0] * rel[:, 1] - edge[1] * rel[:, 0]
        orientation = float(
            edge[0] * (opposite[1] - p0[1]) - edge[1] * (opposite[0] - p0[0])
        )
        if orientation < 0.0:
            cross = -cross
        return cross / float(np.linalg.norm(edge))

    d_ab = edge_distance(a, b, c)
    d_bc = edge_distance(b, c, a)
    d_ca = edge_distance(c, a, b)
    inside = (d_ab >= 0.0) & (d_bc >= 0.0) & (d_ca >= 0.0)
    edge_distance_value = np.minimum(np.minimum(d_ab, d_bc), d_ca)

    seg_ab = _segment_distance(points, a, b)
    seg_bc = _segment_distance(points, b, c)
    seg_ca = _segment_distance(points, c, a)
    outside_distance = np.minimum(np.minimum(seg_ab, seg_bc), seg_ca)
    return np.where(inside, -edge_distance_value, outside_distance)


def _segment_distance(points: np.ndarray, a: np.ndarray, b: np.ndarray) -> np.ndarray:
    ab = b - a
    denom = float(np.dot(ab, ab))
    if denom <= EPS:
        return np.linalg.norm(points - a, axis=1)
    t = ((points[:, 0] - a[0]) * ab[0] + (points[:, 1] - a[1]) * ab[1]) / denom
    t = np.clip(t, 0.0, 1.0)
    closest = a[None, :] + t[:, None] * ab[None, :]
    return np.linalg.norm(points - closest, axis=1)
