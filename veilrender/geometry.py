"""Geometry primitives shared by rendering, projection, and reports."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .scene import point_inside_layer

_EPS = 1e-12


def normalize(vector: np.ndarray) -> np.ndarray:
    length = float(np.linalg.norm(vector))
    if length <= _EPS:
        return np.zeros_like(vector, dtype=np.float64)
    return np.asarray(vector, dtype=np.float64) / length


def safe_normalize(vectors: np.ndarray, axis: int = -1) -> np.ndarray:
    lengths = np.linalg.norm(vectors, axis=axis, keepdims=True)
    return np.divide(vectors, lengths, out=np.zeros_like(vectors), where=lengths > _EPS)


def smoothstep(edge0: float, edge1: float, values: np.ndarray) -> np.ndarray:
    if edge1 <= edge0:
        return (values >= edge0).astype(np.float64)
    x = np.clip((values - edge0) / (edge1 - edge0), 0.0, 1.0)
    return x * x * (3.0 - 2.0 * x)


@dataclass
class LayerGeometry:
    definition: dict[str, Any]
    ordinal: int
    layer: int
    layer_id: str
    kind: str
    vertices: np.ndarray
    triangles: np.ndarray
    triangle_normals: np.ndarray
    triangle_centroids: np.ndarray
    normal: np.ndarray
    centroid: np.ndarray
    reference: np.ndarray
    u_axis: np.ndarray
    v_axis: np.ndarray
    uv_bounds: tuple[float, float, float, float]
    texture: np.ndarray | None = None

    def uv(self, points: np.ndarray) -> np.ndarray:
        relative = np.asarray(points, dtype=np.float64) - self.reference
        return np.stack((relative @ self.u_axis, relative @ self.v_axis), axis=-1)

    def world_from_uv(self, u: np.ndarray | float, v: np.ndarray | float) -> np.ndarray:
        u_arr = np.asarray(u, dtype=np.float64)
        v_arr = np.asarray(v, dtype=np.float64)
        return self.reference + u_arr[..., None] * self.u_axis + v_arr[..., None] * self.v_axis

    def inside_mesh(self, points: np.ndarray, tolerance: float = 1e-9) -> np.ndarray:
        points = np.asarray(points, dtype=np.float64)
        original_shape = points.shape[:-1]
        flat = points.reshape(-1, 3)
        uv_points = self.uv(flat)
        inside = np.zeros(flat.shape[0], dtype=bool)
        for triangle in self.triangles:
            tri_uv = self.uv(self.vertices[triangle])
            a = tri_uv[0]
            b = tri_uv[1]
            c = tri_uv[2]
            v0 = b - a
            v1 = c - a
            v2 = uv_points - a
            denominator = float(v0[0] * v1[1] - v1[0] * v0[1])
            if abs(denominator) <= 1e-18:
                continue
            q = (v2[:, 0] * v1[1] - v1[0] * v2[:, 1]) / denominator
            r = (v0[0] * v2[:, 1] - v2[:, 0] * v0[1]) / denominator
            inside |= (q >= -tolerance) & (r >= -tolerance) & (q + r <= 1.0 + tolerance)
        return inside.reshape(original_shape)

    def signed_distance_to_uv_rect_edge(self, points: np.ndarray) -> np.ndarray:
        uv_points = self.uv(np.asarray(points, dtype=np.float64))
        u_min, u_max, v_min, v_max = self.uv_bounds
        return np.minimum.reduce(
            (
                uv_points[..., 0] - u_min,
                u_max - uv_points[..., 0],
                uv_points[..., 1] - v_min,
                v_max - uv_points[..., 1],
            )
        )


def build_layer_geometries(scene: dict[str, Any]) -> list[LayerGeometry]:
    camera_position = np.asarray(scene["camera"]["position"], dtype=np.float64)
    geometries: list[LayerGeometry] = []
    for ordinal, layer in enumerate(scene["layers"]):
        vertices = np.asarray(layer["mesh"]["vertices"], dtype=np.float64)
        triangles = np.asarray(layer["mesh"]["triangles"], dtype=np.int64)
        centroids = np.mean(vertices[triangles], axis=1)
        normals = np.cross(
            vertices[triangles[:, 1]] - vertices[triangles[:, 0]],
            vertices[triangles[:, 2]] - vertices[triangles[:, 0]],
        )
        lengths = np.linalg.norm(normals, axis=1)
        normals = normals / lengths[:, None]
        normal = normals[0]
        centroid = np.mean(vertices, axis=0)
        if float(np.dot(normal, camera_position - centroid)) < 0.0:
            normal = -normal
            normals = -normals
        up = np.asarray([0.0, 1.0, 0.0])
        u_axis = np.cross(up, normal)
        if float(np.linalg.norm(u_axis)) <= 1e-8:
            candidate = np.asarray([1.0, 0.0, 0.0])
            u_axis = candidate - normal * float(np.dot(candidate, normal))
        u_axis = normalize(u_axis)
        v_axis = normalize(np.cross(normal, u_axis))
        relative = vertices - vertices[0]
        u_values = relative @ u_axis
        v_values = relative @ v_axis
        uv_bounds = (
            float(np.min(u_values)),
            float(np.max(u_values)),
            float(np.min(v_values)),
            float(np.max(v_values)),
        )
        geometries.append(
            LayerGeometry(
                definition=layer,
                ordinal=ordinal,
                layer=int(layer["layer"]),
                layer_id=str(layer["id"]),
                kind=str(layer["kind"]),
                vertices=vertices,
                triangles=triangles,
                triangle_normals=normals,
                triangle_centroids=centroids,
                normal=normal,
                centroid=centroid,
                reference=vertices[0].copy(),
                u_axis=u_axis,
                v_axis=v_axis,
                uv_bounds=uv_bounds,
            )
        )
    return geometries


def camera_rays(
    camera: dict[str, Any],
    image_width: int,
    image_height: int,
    x0: int,
    y0: int,
    tile_width: int,
    tile_height: int,
) -> tuple[np.ndarray, np.ndarray, int]:
    """Return origins/directions shaped (S,H,W,3) and the sample count."""

    position = np.asarray(camera["position"], dtype=np.float64)
    target = np.asarray(camera["target"], dtype=np.float64)
    up = np.asarray(camera["up"], dtype=np.float64)
    forward = normalize(target - position)
    right = normalize(np.cross(forward, up))
    true_up = normalize(np.cross(right, forward))
    sample_grid = int(camera["samples"])
    tan_y = math.tan(math.radians(float(camera["fov_y_deg"])) * 0.5)
    aspect = float(image_width) / float(image_height)

    x_coordinates = np.arange(x0, x0 + tile_width, dtype=np.float64)
    y_coordinates = np.arange(y0, y0 + tile_height, dtype=np.float64)
    if sample_grid == 1:
        x_offsets = np.asarray([0.5])
        y_offsets = np.asarray([0.5])
    else:
        x_offsets = (np.arange(sample_grid, dtype=np.float64) + 0.5) / sample_grid
        y_offsets = (np.arange(sample_grid, dtype=np.float64) + 0.5) / sample_grid

    all_directions: list[np.ndarray] = []
    for y_offset in y_offsets:
        for x_offset in x_offsets:
            px = x_coordinates[None, :] + x_offset
            py = y_coordinates[:, None] + y_offset
            ndc_x = (2.0 * px / float(image_width) - 1.0) * aspect * tan_y
            ndc_y = (1.0 - 2.0 * py / float(image_height)) * tan_y
            direction = (
                forward[None, None, :]
                + right[None, None, :] * ndc_x[..., None]
                + true_up[None, None, :] * ndc_y[..., None]
            )
            all_directions.append(safe_normalize(direction))
    directions = np.stack(all_directions, axis=0)
    origins = np.broadcast_to(position, directions.shape).copy()
    return origins, directions, sample_grid * sample_grid


def _intersect_triangle(
    origins: np.ndarray,
    directions: np.ndarray,
    a: np.ndarray,
    b: np.ndarray,
    c: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    edge1 = b - a
    edge2 = c - a
    pvec = np.cross(directions, edge2)
    determinant = pvec @ edge1
    valid = np.abs(determinant) > 1e-12
    inverse = np.zeros_like(determinant)
    inverse[valid] = 1.0 / determinant[valid]
    tvec = origins - a
    u = np.sum(tvec * pvec, axis=1) * inverse
    qvec = np.cross(tvec, edge1)
    v = np.sum(directions * qvec, axis=1) * inverse
    t = (qvec @ edge2) * inverse
    bary_valid = (u >= 0.0) & (v >= 0.0) & (u + v <= 1.0)
    return t, u, v, valid & bary_valid


def raycast_layer(
    origins: np.ndarray,
    directions: np.ndarray,
    geometry: LayerGeometry,
    near: float,
    far: float,
) -> dict[str, np.ndarray]:
    flat_origins = origins.reshape(-1, 3)
    flat_directions = directions.reshape(-1, 3)
    best_t = np.full(flat_origins.shape[0], np.inf, dtype=np.float64)
    best_triangle = np.full(flat_origins.shape[0], -1, dtype=np.int64)
    best_u = np.zeros(flat_origins.shape[0], dtype=np.float64)
    best_v = np.zeros(flat_origins.shape[0], dtype=np.float64)
    for triangle_index, triangle in enumerate(geometry.triangles):
        a, b, c = geometry.vertices[triangle]
        t, u, v, valid = _intersect_triangle(flat_origins, flat_directions, a, b, c)
        valid &= (t >= near) & (t <= far) & (t < best_t)
        best_t[valid] = t[valid]
        best_triangle[valid] = triangle_index
        best_u[valid] = u[valid]
        best_v[valid] = v[valid]
    hit = best_triangle >= 0
    points = flat_origins + flat_directions * best_t[:, None]
    normals = np.zeros_like(points)
    normals[hit] = geometry.triangle_normals[best_triangle[hit]]
    points[~hit] = np.nan
    normals[~hit] = np.nan
    return {
        "hit": hit,
        "t": best_t,
        "triangle": best_triangle,
        "bary_u": best_u,
        "bary_v": best_v,
        "point": points,
        "normal": normals,
    }


def sample_texture(texture: np.ndarray, uv: np.ndarray, repeat: float) -> np.ndarray:
    height, width = texture.shape
    x = ((uv[:, 0] * repeat) % 1.0) * width - 0.5
    y = ((uv[:, 1] * repeat) % 1.0) * height - 0.5
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    fx = x - x0
    fy = y - y0
    x0 %= width
    y0 %= height
    x1 = (x0 + 1) % width
    y1 = (y0 + 1) % height
    values = texture.astype(np.float64) / 255.0
    top = values[y0, x0] * (1.0 - fx) + values[y0, x1] * fx
    bottom = values[y1, x0] * (1.0 - fx) + values[y1, x1] * fx
    return top * (1.0 - fy) + bottom * fy


def _nearest_line_distance(coordinate: np.ndarray, pitch: float, offset: float) -> np.ndarray:
    shifted = (coordinate - offset) / pitch
    return np.abs(shifted - np.rint(shifted)) * pitch


def lattice_pattern(
    geometry: LayerGeometry,
    points: np.ndarray,
    *,
    softness: float | np.ndarray | None = None,
) -> tuple[np.ndarray, np.ndarray]:
    """Return bar coverage and distance to the nearest bar centerline."""

    layer = geometry.definition
    pattern = layer["pattern"]
    pitch = float(pattern["pitch"])
    width = float(layer["width"])
    edge = (
        float(pattern["edge_softness"])
        if softness is None
        else np.asarray(softness, dtype=np.float64)
    )
    seed = int(layer["pattern_seed"])
    offset = (seed * 0.0137) % pitch
    uv = geometry.uv(np.asarray(points, dtype=np.float64).reshape(-1, 3))
    if pattern["style"] == "diamond":
        inverse_sqrt_two = 1.0 / math.sqrt(2.0)
        first = (uv[:, 0] + uv[:, 1]) * inverse_sqrt_two
        second = (uv[:, 0] - uv[:, 1]) * inverse_sqrt_two
    else:
        first = uv[:, 0]
        second = uv[:, 1]
    d_first = _nearest_line_distance(first, pitch, offset)
    d_second = _nearest_line_distance(second, pitch, offset)
    distance = np.minimum(d_first, d_second)
    edge_array = np.broadcast_to(edge, distance.shape)
    lower = width * 0.5 - edge_array * 0.5
    ratio = np.divide(
        distance - lower,
        edge_array,
        out=np.where(distance <= width * 0.5, 0.0, 1.0),
        where=edge_array > 1e-15,
    )
    ratio = np.clip(ratio, 0.0, 1.0)
    antialiased = ratio * ratio * (3.0 - 2.0 * ratio)
    coverage = np.where(edge_array > 1e-15, 1.0 - antialiased, distance <= width * 0.5)
    return np.clip(coverage, 0.0, 1.0), distance


def plane_intersection_parameter(
    origins: np.ndarray,
    directions: np.ndarray,
    geometry: LayerGeometry,
) -> np.ndarray:
    denominator = directions @ geometry.normal
    numerator = (geometry.reference - origins) @ geometry.normal
    result = np.full(denominator.shape, np.nan, dtype=np.float64)
    valid = np.abs(denominator) > 1e-10
    result[valid] = numerator[valid] / denominator[valid]
    return result


def point_inside_geometry_definition(point: np.ndarray, geometry: LayerGeometry) -> bool:
    return point_inside_layer(point, geometry.vertices, geometry.triangles)

