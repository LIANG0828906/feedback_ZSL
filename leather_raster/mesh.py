from __future__ import annotations

from dataclasses import dataclass

import numpy as np


def triangle_areas(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    v0 = vertices[triangles[:, 0]]
    v1 = vertices[triangles[:, 1]]
    v2 = vertices[triangles[:, 2]]
    return 0.5 * np.linalg.norm(np.cross(v1 - v0, v2 - v0), axis=1)


def face_normals(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    v0 = vertices[triangles[:, 0]]
    v1 = vertices[triangles[:, 1]]
    v2 = vertices[triangles[:, 2]]
    normals = np.cross(v1 - v0, v2 - v0)
    lengths = np.linalg.norm(normals, axis=1)
    safe = np.maximum(lengths, 1e-30)
    normals = normals / safe[:, None]
    normals[lengths <= 1e-30] = np.array([0.0, 0.0, 1.0])
    return normals


def _point_segment_distance(point: np.ndarray, a: np.ndarray, b: np.ndarray) -> np.ndarray:
    ab = b - a
    denominator = np.einsum("ij,ij->i", ab, ab)
    safe = np.maximum(denominator, 1e-30)
    t = np.einsum("ij,ij->i", point - a, ab) / safe
    t = np.clip(t, 0.0, 1.0)
    closest = a + t[:, None] * ab
    return np.linalg.norm(point - closest, axis=1)


def point_triangle_distance(
    point: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
) -> np.ndarray:
    """Distance from one point to every triangle in a mesh."""

    p = np.asarray(point, dtype=np.float64)
    a = vertices[triangles[:, 0]]
    b = vertices[triangles[:, 1]]
    c = vertices[triangles[:, 2]]
    ab = b - a
    ac = c - a
    ap = p[None, :] - a
    normal = np.cross(ab, ac)
    normal_len2 = np.einsum("ij,ij->i", normal, normal)
    safe = np.maximum(normal_len2, 1e-30)
    signed_plane = np.einsum("ij,ij->i", ap, normal) / np.sqrt(safe)
    projected = p[None, :] - (
        np.einsum("ij,ij->i", ap, normal) / safe
    )[:, None] * normal

    def edge_test(x: np.ndarray, y: np.ndarray, q: np.ndarray) -> np.ndarray:
        return np.einsum("ij,ij->i", np.cross(y - x, q - x), normal)

    c0 = edge_test(a, b, projected)
    c1 = edge_test(b, c, projected)
    c2 = edge_test(c, a, projected)
    inside = (c0 >= -1e-12) & (c1 >= -1e-12) & (c2 >= -1e-12)
    plane_distance = np.abs(signed_plane)

    edge_distance = np.minimum.reduce(
        [
            _point_segment_distance(projected, a, b),
            _point_segment_distance(projected, b, c),
            _point_segment_distance(projected, c, a),
        ]
    )
    return np.where(inside, plane_distance, np.sqrt(plane_distance**2 + edge_distance**2))


def point_inside_triangular_mesh(
    point: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
    tolerance: float,
) -> bool:
    """Return True on/inside a surface or inside a closed oriented mesh.

    Exact or near-surface points are treated as inside. For closed meshes a
    generalized winding number handles arbitrary orientation and concave shapes.
    """

    if len(triangles) == 0:
        return False
    distances = point_triangle_distance(point, vertices, triangles)
    if float(np.min(distances)) <= tolerance:
        return True

    a = vertices[triangles[:, 0]] - point
    b = vertices[triangles[:, 1]] - point
    c = vertices[triangles[:, 2]] - point
    la = np.maximum(np.linalg.norm(a, axis=1), 1e-30)
    lb = np.maximum(np.linalg.norm(b, axis=1), 1e-30)
    lc = np.maximum(np.linalg.norm(c, axis=1), 1e-30)
    numerator = np.einsum("ij,ij->i", a, np.cross(b, c))
    denominator = (
        la * lb * lc
        + np.einsum("ij,ij->i", a, b) * lc
        + np.einsum("ij,ij->i", b, c) * la
        + np.einsum("ij,ij->i", c, a) * lb
    )
    solid_angles = 2.0 * np.arctan2(numerator, denominator)
    winding = float(np.sum(solid_angles) / (4.0 * np.pi))
    return abs(winding) > 0.5


@dataclass(frozen=True)
class PlaneFrame:
    origin: np.ndarray
    normal: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray
    bounds_u: tuple[float, float]
    bounds_v: tuple[float, float]


def fit_plane_frame(vertices: np.ndarray, fallback_normal: np.ndarray | None = None) -> PlaneFrame:
    """Fit a stable least-squares plane and a deterministic 2D frame."""

    origin = np.mean(vertices, axis=0)
    centered = vertices - origin
    covariance = centered.T @ centered
    _, eigenvectors = np.linalg.eigh(covariance)
    normal = eigenvectors[:, 0]
    length = float(np.linalg.norm(normal))
    if length <= 1e-15:
        normal = np.array([0.0, 0.0, 1.0])
    else:
        normal = normal / length
    if fallback_normal is not None and float(np.dot(normal, fallback_normal)) < 0.0:
        normal = -normal
    dominant_axis = int(np.argmax(np.abs(normal)))
    if float(normal[dominant_axis]) < 0.0:
        normal = -normal

    axis_candidates = np.array(
        [
            [1.0, 0.0, 0.0],
            [0.0, 1.0, 0.0],
            [0.0, 0.0, 1.0],
        ]
    )
    axis_u = axis_candidates[int(np.argmin(np.abs(axis_candidates @ normal)))]
    axis_u = axis_u - normal * float(np.dot(axis_u, normal))
    axis_u = axis_u / max(float(np.linalg.norm(axis_u)), 1e-30)
    axis_v = np.cross(normal, axis_u)
    axis_v = axis_v / max(float(np.linalg.norm(axis_v)), 1e-30)
    rel = vertices - origin
    u = rel @ axis_u
    v = rel @ axis_v
    return PlaneFrame(
        origin=origin,
        normal=normal,
        axis_u=axis_u,
        axis_v=axis_v,
        bounds_u=(float(np.min(u)), float(np.max(u))),
        bounds_v=(float(np.min(v)), float(np.max(v))),
    )

