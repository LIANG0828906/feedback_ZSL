from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .scene import Layer, Mesh


def _affine_uv_mapping(mesh: Mesh) -> tuple[np.ndarray, np.ndarray] | None:
    vertices = mesh.vertices
    uvs = mesh.uvs
    for triangle in mesh.triangles:
        a, b, c = (int(triangle[0]), int(triangle[1]), int(triangle[2]))
        uv_matrix = np.stack((uvs[b] - uvs[a], uvs[c] - uvs[a]), axis=1)
        if abs(float(np.linalg.det(uv_matrix))) <= 1e-12:
            continue
        normal = np.cross(vertices[b] - vertices[a], vertices[c] - vertices[a])
        normal_length = float(np.linalg.norm(normal))
        if normal_length <= 1e-15:
            continue
        normal = normal / normal_length
        first_axis = vertices[b] - vertices[a]
        first_axis = first_axis - normal * float(np.dot(first_axis, normal))
        first_length = float(np.linalg.norm(first_axis))
        if first_length <= 1e-15:
            continue
        e1 = first_axis / first_length
        e2 = np.cross(normal, e1)
        basis = np.stack((e1, e2), axis=1)
        local = (vertices[[a, b, c]] - vertices[a]) @ basis
        local_matrix = np.stack((local[1], local[2]), axis=1)
        if abs(float(np.linalg.det(local_matrix))) <= 1e-15:
            continue
        affine = np.stack((uvs[b] - uvs[a], uvs[c] - uvs[a]), axis=1) @ np.linalg.inv(
            local_matrix
        )
        offset = uvs[a] - affine @ local[0]

        mapped = (vertices - vertices[a]) @ basis @ affine.T + offset
        residual = float(np.max(np.abs(mapped - uvs)))
        if residual <= 1e-7:
            return affine, offset
    return None


@dataclass(frozen=True)
class PlanarMapper:
    origin: np.ndarray
    normal: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray
    affine: np.ndarray
    offset: np.ndarray
    uv_min: np.ndarray
    uv_max: np.ndarray
    full_uv_rectangle: bool
    uv_triangles: np.ndarray

    def point_to_uv(self, points: np.ndarray) -> np.ndarray:
        local = (points - self.origin) @ np.stack((self.axis_u, self.axis_v), axis=1)
        return local @ self.affine.T + self.offset

    def ray_plane_uv(
        self, origins: np.ndarray, directions: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        denominator = directions @ self.normal
        safe = np.abs(denominator) > 1e-14
        plane_offset = float(np.dot(self.origin, self.normal))
        ray_offset = origins @ self.normal
        t = np.zeros_like(denominator, dtype=np.float64)
        t[safe] = (plane_offset - ray_offset[safe]) / denominator[safe]
        points = origins + directions * t[..., None]
        uv = self.point_to_uv(points)
        inside = (
            (uv[..., 0] >= self.uv_min[0] - 1e-12)
            & (uv[..., 0] <= self.uv_max[0] + 1e-12)
            & (uv[..., 1] >= self.uv_min[1] - 1e-12)
            & (uv[..., 1] <= self.uv_max[1] + 1e-12)
        )
        if not self.full_uv_rectangle:
            inside &= self.contains_uv(uv[..., 0], uv[..., 1])
        return t, uv, inside & safe

    def contains_uv(self, u: np.ndarray, v: np.ndarray) -> np.ndarray:
        points = np.stack((u, v), axis=-1)
        inside = np.zeros(u.shape, dtype=bool)
        epsilon = 1e-10
        for triangle in self.uv_triangles:
            a, b, c = triangle
            ab = b - a
            ac = c - a
            ap = points - a
            cross_ab = ab[0] * ap[..., 1] - ab[1] * ap[..., 0]
            cross_ac = ac[0] * (points[..., 1] - a[1]) - ac[1] * (points[..., 0] - a[0])
            cross_bc = (c[0] - b[0]) * (points[..., 1] - b[1]) - (
                c[1] - b[1]
            ) * (points[..., 0] - b[0])
            sign = float(np.sign(np.cross(b - a, c - a)))
            inside |= (
                (sign * cross_ab >= -epsilon)
                & (sign * cross_ac >= -epsilon)
                & (sign * cross_bc >= -epsilon)
            )
        return inside


def build_planar_mapper(mesh: Mesh) -> PlanarMapper | None:
    first = mesh.vertices[mesh.triangles[0]]
    normal = np.cross(first[1] - first[0], first[2] - first[0])
    normal_length = float(np.linalg.norm(normal))
    if normal_length <= 1e-15:
        return None
    normal = normal / normal_length
    origin = mesh.vertices[int(mesh.triangles[0][0])].copy()
    distances = (mesh.vertices - origin) @ normal
    tolerance = max(1e-10, float(np.max(np.abs(mesh.vertices))) * 1e-9)
    if float(np.max(np.abs(distances))) > tolerance:
        return None

    mapping = _affine_uv_mapping(mesh)
    if mapping is None:
        return None
    affine, offset = mapping
    first_axis = first[1] - first[0]
    first_axis = first_axis - normal * float(np.dot(first_axis, normal))
    first_length = float(np.linalg.norm(first_axis))
    if first_length <= 1e-15:
        return None
    axis_u = first_axis / first_length
    axis_v = np.cross(normal, axis_u)
    uv_min = np.min(mesh.uvs, axis=0)
    uv_max = np.max(mesh.uvs, axis=0)
    uv_triangles = mesh.uvs[mesh.triangles]
    double_area = 0.0
    for triangle in uv_triangles:
        ab = triangle[1] - triangle[0]
        ac = triangle[2] - triangle[0]
        double_area += abs(float(ab[0] * ac[1] - ab[1] * ac[0]))
    rectangle_area = 2.0 * float(
        (uv_max[0] - uv_min[0]) * (uv_max[1] - uv_min[1])
    )
    full_rectangle = (
        rectangle_area > 0.0
        and abs(double_area - rectangle_area) <= max(1e-8, rectangle_area * 1e-7)
    )
    return PlanarMapper(
        origin=origin,
        normal=normal,
        axis_u=axis_u,
        axis_v=axis_v,
        affine=np.asarray(affine, dtype=np.float64),
        offset=np.asarray(offset, dtype=np.float64),
        uv_min=uv_min,
        uv_max=uv_max,
        full_uv_rectangle=full_rectangle,
        uv_triangles=uv_triangles,
    )


def build_mappers(layers: tuple[Layer, ...]) -> tuple[PlanarMapper | None, ...]:
    return tuple(build_planar_mapper(layer.mesh) for layer in layers)


def ray_mesh_uv_hit(
    origins: np.ndarray,
    directions: np.ndarray,
    mesh: Mesh,
) -> tuple[np.ndarray, np.ndarray]:
    """Generic fallback for non-planar meshes. Returns closest hit and UV."""
    ray_count = origins.shape[0]
    best_t = np.full(ray_count, np.inf, dtype=np.float64)
    best_u = np.zeros(ray_count, dtype=np.float64)
    best_v = np.zeros(ray_count, dtype=np.float64)
    best_hit = np.zeros(ray_count, dtype=bool)
    for triangle, triangle_uv in zip(mesh.triangles, mesh.uvs[mesh.triangles]):
        v0, v1, v2 = mesh.vertices[triangle]
        edge1 = v1 - v0
        edge2 = v2 - v0
        h = np.cross(directions, edge2)
        determinant = np.einsum("ij,ij->i", edge1, h)
        valid = np.abs(determinant) > 1e-14
        inverse = np.zeros_like(determinant)
        inverse[valid] = 1.0 / determinant[valid]
        s = origins - v0
        bary_v = inverse * np.einsum("ij,ij->i", s, h)
        q = np.cross(s, edge1)
        bary_w = inverse * np.einsum("ij,ij->i", directions, q)
        t = inverse * np.einsum("ij,ij->i", edge2, q)
        hit = (
            valid
            & (bary_v >= -1e-10)
            & (bary_w >= -1e-10)
            & (bary_v + bary_w <= 1.0 + 1e-10)
            & (t > 1e-8)
            & (t < 1.0 - 1e-8)
            & (t < best_t)
        )
        if not np.any(hit):
            continue
        bary_u = 1.0 - bary_v - bary_w
        uv = (
            bary_u[:, None] * triangle_uv[0]
            + bary_v[:, None] * triangle_uv[1]
            + bary_w[:, None] * triangle_uv[2]
        )
        best_t[hit] = t[hit]
        best_u[hit] = uv[hit, 0]
        best_v[hit] = uv[hit, 1]
        best_hit[hit] = True
    return best_hit, np.stack((best_u, best_v), axis=1)

