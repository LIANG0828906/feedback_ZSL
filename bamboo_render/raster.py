from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .scene import CameraSpec, ImageSpec, LayerSpec, WallSpec


@dataclass(frozen=True)
class ProjectedTriangle:
    indices: tuple[int, int, int]
    xy: np.ndarray
    z: np.ndarray
    uv: np.ndarray
    world: np.ndarray
    normal: np.ndarray
    strand_id: int
    key: tuple


@dataclass(frozen=True)
class PreparedLayer:
    layer: int
    triangles: tuple[ProjectedTriangle, ...]


@dataclass
class RasterBuffers:
    hit: np.ndarray
    depth: np.ndarray
    uv: np.ndarray
    world: np.ndarray
    normal: np.ndarray


def prepare_layer(layer: LayerSpec, camera: CameraSpec, image: ImageSpec, light_position: np.ndarray) -> PreparedLayer:
    vertices = layer.vertices
    relative = vertices - camera.position
    z_camera = relative @ camera.forward
    tan_half = math.tan(math.radians(camera.fov_y_degrees) * 0.5)
    aspect = image.width / image.height
    x_ndc = (relative @ camera.right) / (z_camera * tan_half * aspect)
    y_ndc = (relative @ camera.true_up) / (z_camera * tan_half)
    screen_x = (x_ndc + 1.0) * 0.5 * image.width
    screen_y = (1.0 - y_ndc) * 0.5 * image.height
    valid_vertices = z_camera > camera.near

    triangles: list[ProjectedTriangle] = []
    for triangle_index, tri_array in enumerate(layer.triangles):
        tri = tuple(int(value) for value in tri_array)
        if not all(valid_vertices[index] for index in tri):
            continue
        p0, p1, p2 = vertices[tri[0]], vertices[tri[1]], vertices[tri[2]]
        normal = np.cross(p1 - p0, p2 - p0)
        normal_length = float(np.linalg.norm(normal))
        if normal_length <= 1e-15:
            continue
        normal = normal / normal_length
        if float(np.dot(normal, light_position - p0)) < 0.0:
            normal = -normal
        key_vertices = tuple(sorted(tuple(float(round(value, 9)) for value in vertices[index]) for index in tri))
        key_uvs = tuple(sorted(tuple(float(round(value, 9)) for value in layer.uvs[index]) for index in tri))
        key = (key_vertices, key_uvs, int(layer.strand_ids[triangle_index]), triangle_index)
        triangles.append(
            ProjectedTriangle(
                indices=tri,
                xy=np.array(
                    [
                        [screen_x[tri[0]], screen_y[tri[0]]],
                        [screen_x[tri[1]], screen_y[tri[1]]],
                        [screen_x[tri[2]], screen_y[tri[2]]],
                    ],
                    dtype=np.float64,
                ),
                z=np.array([z_camera[tri[0]], z_camera[tri[1]], z_camera[tri[2]]], dtype=np.float64),
                uv=np.array([layer.uvs[tri[0]], layer.uvs[tri[1]], layer.uvs[tri[2]]], dtype=np.float64),
                world=np.array([p0, p1, p2], dtype=np.float64),
                normal=normal.astype(np.float64),
                strand_id=int(layer.strand_ids[triangle_index]),
                key=key,
            )
        )
    triangles.sort(key=lambda item: item.key)
    return PreparedLayer(layer=layer.layer, triangles=tuple(triangles))


def rasterize_block(
    prepared: PreparedLayer,
    camera: CameraSpec,
    image: ImageSpec,
    block: tuple[int, int, int, int],
) -> RasterBuffers:
    x0, y0, width, height = block
    hit = np.zeros((height, width), dtype=bool)
    depth = np.full((height, width), np.inf, dtype=np.float64)
    uv = np.zeros((height, width, 2), dtype=np.float64)
    world = np.zeros((height, width, 3), dtype=np.float64)
    normal = np.zeros((height, width, 3), dtype=np.float64)

    pixel_x = (np.arange(x0, x0 + width, dtype=np.float64) + 0.5)[None, :]
    pixel_y = (np.arange(y0, y0 + height, dtype=np.float64) + 0.5)[:, None]

    for triangle in prepared.triangles:
        tri_xy = triangle.xy
        min_x = max(0, int(math.ceil(float(tri_xy[:, 0].min()) - 0.5)))
        max_x = min(image.width - 1, int(math.floor(float(tri_xy[:, 0].max()) - 0.5)))
        min_y = max(0, int(math.ceil(float(tri_xy[:, 1].min()) - 0.5)))
        max_y = min(image.height - 1, int(math.floor(float(tri_xy[:, 1].max()) - 0.5)))
        if min_x > max_x or min_y > max_y:
            continue
        bx0 = max(min_x, x0)
        bx1 = min(max_x, x0 + width - 1)
        by0 = max(min_y, y0)
        by1 = min(max_y, y0 + height - 1)
        if bx0 > bx1 or by0 > by1:
            continue

        x = pixel_x[:, bx0 - x0 : bx1 - x0 + 1]
        y = pixel_y[by0 - y0 : by1 - y0 + 1, :]
        x_grid = np.broadcast_to(x, (by1 - by0 + 1, bx1 - bx0 + 1))
        y_grid = np.broadcast_to(y, (by1 - by0 + 1, bx1 - bx0 + 1))
        ax, ay = tri_xy[0]
        bx, by = tri_xy[1]
        cx, cy = tri_xy[2]
        denominator = (by - cy) * (ax - cx) + (cx - bx) * (ay - cy)
        if abs(denominator) <= 1e-15:
            continue
        b0 = ((by - cy) * (x_grid - cx) + (cx - bx) * (y_grid - cy)) / denominator
        b1 = ((cy - ay) * (x_grid - cx) + (ax - cx) * (y_grid - cy)) / denominator
        b2 = 1.0 - b0 - b1
        inside = (b0 >= -1e-9) & (b1 >= -1e-9) & (b2 >= -1e-9)
        if not np.any(inside):
            continue

        inv_z = b0 / triangle.z[0] + b1 / triangle.z[1] + b2 / triangle.z[2]
        valid = inside & (np.abs(inv_z) > 1e-15)
        if not np.any(valid):
            continue
        candidate_depth = 1.0 / inv_z
        local_depth = depth[by0 - y0 : by1 - y0 + 1, bx0 - x0 : bx1 - x0 + 1]
        update = valid & (candidate_depth < local_depth - 1e-9)
        if not np.any(update):
            continue

        perspective = np.stack([b0 * inv_z, b1 * inv_z, b2 * inv_z], axis=-1)
        perspective = perspective / perspective.sum(axis=-1, keepdims=True)
        candidate_uv = (
            perspective[..., 0:1] * triangle.uv[0]
            + perspective[..., 1:2] * triangle.uv[1]
            + perspective[..., 2:3] * triangle.uv[2]
        )
        candidate_world = (
            perspective[..., 0:1] * triangle.world[0]
            + perspective[..., 1:2] * triangle.world[1]
            + perspective[..., 2:3] * triangle.world[2]
        )

        local_hit = hit[by0 - y0 : by1 - y0 + 1, bx0 - x0 : bx1 - x0 + 1]
        local_uv = uv[by0 - y0 : by1 - y0 + 1, bx0 - x0 : bx1 - x0 + 1]
        local_world = world[by0 - y0 : by1 - y0 + 1, bx0 - x0 : bx1 - x0 + 1]
        local_normal = normal[by0 - y0 : by1 - y0 + 1, bx0 - x0 : bx1 - x0 + 1]
        local_depth[update] = candidate_depth[update]
        local_hit[update] = True
        local_uv[update] = candidate_uv[update]
        local_world[update] = candidate_world[update]
        local_normal[update] = triangle.normal

    return RasterBuffers(hit=hit, depth=depth, uv=uv, world=world, normal=normal)


def render_wall_block(
    wall: WallSpec,
    camera: CameraSpec,
    image: ImageSpec,
    block: tuple[int, int, int, int],
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    x0, y0, width, height = block
    rays = camera_rays(camera, image, x0, y0, width, height)
    denominator = rays @ wall.normal
    numerator = float(np.dot(wall.point - camera.position, wall.normal))
    t = np.full((height, width), np.inf, dtype=np.float64)
    valid = np.abs(denominator) > 1e-12
    t[valid] = numerator / denominator[valid]
    hit = valid & (t > camera.near) & (t < camera.far)
    world = camera.position[None, None, :] + t[..., None] * rays
    normal = np.broadcast_to(wall.normal, world.shape).copy()
    view_direction = camera.position[None, None, :] - world
    facing = np.sum(normal * view_direction, axis=-1) >= 0.0
    normal[~facing] *= -1.0
    return hit, world, normal


def camera_rays(
    camera: CameraSpec,
    image: ImageSpec,
    x0: int,
    y0: int,
    width: int,
    height: int,
) -> np.ndarray:
    tan_half = math.tan(math.radians(camera.fov_y_degrees) * 0.5)
    aspect = image.width / image.height
    xs = (np.arange(x0, x0 + width, dtype=np.float64) + 0.5) / image.width
    ys = (np.arange(y0, y0 + height, dtype=np.float64) + 0.5) / image.height
    ndc_x = (2.0 * xs - 1.0) * tan_half * aspect
    ndc_y = (1.0 - 2.0 * ys) * tan_half
    direction = (
        camera.forward[None, None, :]
        + ndc_x[None, :, None] * camera.right[None, None, :]
        + ndc_y[:, None, None] * camera.true_up[None, None, :]
    )
    direction /= np.linalg.norm(direction, axis=-1, keepdims=True)
    return direction
