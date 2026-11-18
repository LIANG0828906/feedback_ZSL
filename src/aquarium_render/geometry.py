"""Deterministic mesh construction and ray/triangle utilities."""

from __future__ import annotations

import math
from typing import Any, Iterable

import numpy as np


def _quad(a: int, b: int, c: int, d: int) -> list[list[int]]:
    return [[a, b, c], [a, c, d]]


def arc_shell_mesh(
    inner_radius: float,
    outer_radius: float,
    z0: float,
    z1: float,
    angle0_deg: float,
    angle1_deg: float,
    radial_segments: int,
    height_segments: int,
) -> dict[str, Any]:
    """Build a closed, triangulated arc-shell volume.

    The shell spans an annular sector. It has outer/inner curved walls, top and
    bottom annular caps, and two radial end caps. Vertices are emitted in a
    fixed order that depends only on the numeric arguments.
    """

    if radial_segments < 2 or height_segments < 1:
        raise ValueError("arc shell resolution is too low")
    a0 = math.radians(angle0_deg)
    a1 = math.radians(angle1_deg)
    angles = np.linspace(a0, a1, radial_segments + 1)
    heights = np.linspace(z0, z1, height_segments + 1)

    vertices: list[list[float]] = []
    triangles: list[list[int]] = []

    def add_vertex(x: float, y: float, z: float) -> int:
        vertices.append([x, y, z])
        return len(vertices) - 1

    # Outer surface.
    grid: list[list[int]] = []
    for z in heights:
        row = []
        for angle in angles:
            row.append(
                add_vertex(
                    outer_radius * math.cos(float(angle)),
                    outer_radius * math.sin(float(angle)),
                    float(z),
                )
            )
        grid.append(row)
    for j in range(height_segments):
        for i in range(radial_segments):
            triangles += _quad(grid[j][i], grid[j][i + 1], grid[j + 1][i + 1], grid[j + 1][i])

    # Inner surface, opposite winding.
    grid = []
    for z in heights:
        row = []
        for angle in angles:
            row.append(
                add_vertex(
                    inner_radius * math.cos(float(angle)),
                    inner_radius * math.sin(float(angle)),
                    float(z),
                )
            )
        grid.append(row)
    for j in range(height_segments):
        for i in range(radial_segments):
            triangles += _quad(grid[j][i], grid[j + 1][i], grid[j + 1][i + 1], grid[j][i + 1])

    # Bottom and top annular caps.
    for z, flip in ((z0, False), (z1, True)):
        grid = []
        for radius in (inner_radius, outer_radius):
            row = []
            for angle in angles:
                row.append(
                    add_vertex(
                        radius * math.cos(float(angle)),
                        radius * math.sin(float(angle)),
                        float(z),
                    )
                )
            grid.append(row)
        for i in range(radial_segments):
            if flip:
                triangles += _quad(grid[0][i], grid[0][i + 1], grid[1][i + 1], grid[1][i])
            else:
                triangles += _quad(grid[0][i], grid[1][i], grid[1][i + 1], grid[0][i + 1])

    # End caps.
    for angle, flip in ((a0, False), (a1, True)):
        grid = []
        for z in heights:
            row = [
                add_vertex(
                    inner_radius * math.cos(angle),
                    inner_radius * math.sin(angle),
                    float(z),
                ),
                add_vertex(
                    outer_radius * math.cos(angle),
                    outer_radius * math.sin(angle),
                    float(z),
                ),
            ]
            grid.append(row)
        for j in range(height_segments):
            if flip:
                triangles += _quad(grid[j][0], grid[j][1], grid[j + 1][1], grid[j + 1][0])
            else:
                triangles += _quad(grid[j][0], grid[j + 1][0], grid[j + 1][1], grid[j][1])

    return {
        "vertices": [[round(v, 10) for v in vertex] for vertex in vertices],
        "triangles": triangles,
    }


def annulus_mesh(
    inner_radius: float,
    outer_radius: float,
    z: float,
    angle0_deg: float,
    angle1_deg: float,
    radial_segments: int,
    angular_segments: int,
) -> dict[str, Any]:
    a0 = math.radians(angle0_deg)
    a1 = math.radians(angle1_deg)
    angles = np.linspace(a0, a1, angular_segments + 1)
    radii = np.linspace(inner_radius, outer_radius, radial_segments + 1)
    vertices: list[list[float]] = []
    triangles: list[list[int]] = []
    grid: list[list[int]] = []
    for radius in radii:
        row = []
        for angle in angles:
            row.append(len(vertices))
            vertices.append(
                [
                    round(float(radius * math.cos(angle)), 10),
                    round(float(radius * math.sin(angle)), 10),
                    round(float(z), 10),
                ]
            )
        grid.append(row)
    for j in range(radial_segments):
        for i in range(angular_segments):
            triangles += _quad(grid[j][i], grid[j][i + 1], grid[j + 1][i + 1], grid[j + 1][i])
    return {"vertices": vertices, "triangles": triangles}


def quad_mesh(points: Iterable[Iterable[float]]) -> dict[str, Any]:
    vertices = [[round(float(x), 10) for x in point] for point in points]
    if len(vertices) != 4:
        raise ValueError("a fish quad needs exactly four points")
    return {"vertices": vertices, "triangles": [[0, 1, 2], [0, 2, 3]]}


def triangle_area2(a: np.ndarray, b: np.ndarray, c: np.ndarray) -> np.ndarray:
    return np.linalg.norm(np.cross(b - a, c - a), axis=-1)


def mesh_arrays(mesh: dict[str, Any]) -> tuple[np.ndarray, np.ndarray]:
    vertices = np.asarray(mesh["vertices"], dtype=np.float64)
    triangles = np.asarray(mesh["triangles"], dtype=np.int64)
    return vertices, triangles


def ray_mesh_intersections(
    origins: np.ndarray,
    directions: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
    epsilon: float = 1e-8,
) -> np.ndarray:
    """Vectorised Moller-Trumbore intersections.

    Returns an ``(N, M)`` array. Non-hits are ``+inf``.
    """

    v0 = vertices[triangles[:, 0]]
    v1 = vertices[triangles[:, 1]]
    v2 = vertices[triangles[:, 2]]
    edge1 = v1 - v0
    edge2 = v2 - v0
    pvec = np.cross(directions[:, None, :], edge2[None, :, :])
    det = np.einsum("mj,nmj->nm", edge1, pvec)
    inv_det = np.zeros_like(det)
    np.divide(1.0, det, out=inv_det, where=np.abs(det) > epsilon)
    tvec = origins[:, None, :] - v0[None, :, :]
    u = np.einsum("nmj,nmj->nm", tvec, pvec) * inv_det
    qvec = np.cross(tvec, edge1[None, :, :])
    v = np.einsum("nj,nmj->nm", directions, qvec) * inv_det
    t = np.einsum("mj,nmj->nm", edge2, qvec) * inv_det
    valid = (
        (np.abs(det) > epsilon)
        & (u >= -epsilon)
        & (u <= 1.0 + epsilon)
        & (v >= -epsilon)
        & (u + v <= 1.0 + epsilon)
        & (t > epsilon)
    )
    return np.where(valid, t, np.inf)


def sorted_shell_interval(t_hits: np.ndarray, epsilon: float = 1e-7) -> tuple[np.ndarray, np.ndarray]:
    """Return the first two distinct hits as a ray interval."""

    ordered = np.sort(t_hits, axis=1)
    first = ordered[:, 0]
    second = ordered[:, 1]
    finite_pair = np.isfinite(first) & np.isfinite(second)
    difference = np.zeros_like(first)
    np.subtract(second, first, out=difference, where=finite_pair)
    duplicate = finite_pair & (np.abs(difference) <= epsilon)
    if np.any(duplicate):
        third = ordered[:, 2]
        second = np.where(duplicate, third, second)
    return first, second


def first_hit(t_hits: np.ndarray) -> np.ndarray:
    return np.min(t_hits, axis=1)


def point_in_closed_mesh(point: np.ndarray, vertices: np.ndarray, triangles: np.ndarray) -> bool:
    """Parity test using a fixed non-axis-aligned ray."""

    direction = np.array([0.3713906763541037, 0.5570860145311556, 0.7427813527082074])
    hits = ray_mesh_intersections(
        point[None, :],
        direction[None, :],
        vertices,
        triangles,
        epsilon=1e-9,
    )[0]
    finite = np.isfinite(hits)
    if not np.any(finite):
        return False
    unique = np.sort(hits[finite])
    keep = np.ones_like(unique, dtype=bool)
    keep[1:] = np.diff(unique) > 1e-7
    return bool(np.count_nonzero(keep) % 2 == 1)


def face_normals(vertices: np.ndarray, triangles: np.ndarray) -> np.ndarray:
    a = vertices[triangles[:, 0]]
    b = vertices[triangles[:, 1]]
    c = vertices[triangles[:, 2]]
    normals = np.cross(b - a, c - a)
    lengths = np.linalg.norm(normals, axis=1)
    return normals / np.maximum(lengths[:, None], 1e-15)

