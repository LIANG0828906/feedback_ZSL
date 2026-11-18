"""Small deterministic 3-D helpers used by the renderer."""

from __future__ import annotations

import math

import numpy as np

EPS = 1.0e-12


def as_vec3(value: object) -> np.ndarray:
    return np.asarray(value, dtype=np.float64).reshape(3)


def normalize(v: np.ndarray, *, fallback: np.ndarray | None = None) -> np.ndarray:
    length = float(np.linalg.norm(v))
    if length <= EPS:
        if fallback is None:
            raise ValueError("zero-length vector")
        return np.asarray(fallback, dtype=np.float64).copy()
    return np.asarray(v, dtype=np.float64) / length


def smoothstep(edge0: float, edge1: float, x: np.ndarray | float) -> np.ndarray | float:
    if edge1 <= edge0:
        return np.asarray(x >= edge1, dtype=np.float64)
    t = np.clip((x - edge0) / (edge1 - edge0), 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def luminance(rgb: np.ndarray) -> np.ndarray:
    rgb = np.asarray(rgb)
    return 0.2126 * rgb[..., 0] + 0.7152 * rgb[..., 1] + 0.0722 * rgb[..., 2]


def camera_basis(position: np.ndarray, target: np.ndarray, up: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    forward = normalize(target - position)
    right = normalize(np.cross(forward, up))
    true_up = normalize(np.cross(right, forward))
    return forward, right, true_up


def ray_triangle_t(origin: np.ndarray, direction: np.ndarray, v0: np.ndarray, v1: np.ndarray, v2: np.ndarray) -> float | None:
    """Moller-Trumbore intersection distance, or None."""
    edge1 = v1 - v0
    edge2 = v2 - v0
    pvec = np.cross(direction, edge2)
    det = float(np.dot(edge1, pvec))
    if abs(det) < 1.0e-12:
        return None
    inv_det = 1.0 / det
    tvec = origin - v0
    u = float(np.dot(tvec, pvec)) * inv_det
    if u < -1.0e-10 or u > 1.0 + 1.0e-10:
        return None
    qvec = np.cross(tvec, edge1)
    v = float(np.dot(direction, qvec)) * inv_det
    if v < -1.0e-10 or u + v > 1.0 + 1.0e-10:
        return None
    t = float(np.dot(edge2, qvec)) * inv_det
    if t <= 1.0e-9:
        return None
    return t


def point_in_closed_mesh(point: np.ndarray, vertices: np.ndarray, triangles: np.ndarray) -> bool:
    """Parity test with three fixed non-axis rays.

    Closed meshes may have coplanar triangles, so a majority vote avoids a ray
    grazing a shared edge from flipping the result.
    """
    directions = (
        normalize(np.array([1.0, 0.371, 0.529])),
        normalize(np.array([-0.271, 1.0, 0.413])),
        normalize(np.array([0.193, -0.347, 1.0])),
    )
    votes = 0
    for direction in directions:
        hits: list[float] = []
        for tri in triangles:
            t = ray_triangle_t(point, direction, vertices[int(tri[0])], vertices[int(tri[1])], vertices[int(tri[2])])
            if t is not None:
                if not any(abs(t - old) < 1.0e-7 for old in hits):
                    hits.append(t)
        if len(hits) % 2 == 1:
            votes += 1
    return votes >= 2


def mesh_is_closed(triangles: np.ndarray) -> bool:
    if len(triangles) == 0:
        return False
    edges: dict[tuple[int, int], int] = {}
    for tri in triangles:
        a, b, c = (int(tri[0]), int(tri[1]), int(tri[2]))
        for u, v in ((a, b), (b, c), (c, a)):
            if u == v:
                return False
            key = (u, v) if u < v else (v, u)
            edges[key] = edges.get(key, 0) + 1
    return all(count == 2 for count in edges.values())


def triangle_areas(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    a = vertices[triangles[:, 0]]
    b = vertices[triangles[:, 1]]
    c = vertices[triangles[:, 2]]
    return 0.5 * np.linalg.norm(np.cross(b - a, c - a), axis=1)


def triangle_normals(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    a = vertices[triangles[:, 0]]
    b = vertices[triangles[:, 1]]
    c = vertices[triangles[:, 2]]
    normals = np.cross(b - a, c - a)
    lengths = np.linalg.norm(normals, axis=1)
    safe = np.maximum(lengths, 1.0e-30)
    return normals / safe[:, None]


def srgb_encode(value: np.ndarray) -> np.ndarray:
    value = np.clip(np.asarray(value, dtype=np.float64), 0.0, 1.0)
    return np.where(value <= 0.0031308, 12.92 * value, 1.055 * np.power(value, 1.0 / 2.4) - 0.055)


def canonical_float(value: float, digits: int = 10) -> float:
    rounded = round(float(value), digits)
    return 0.0 if abs(rounded) < 1.0e-15 else rounded


def finite_float(value: object) -> bool:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return False
    return math.isfinite(number)
