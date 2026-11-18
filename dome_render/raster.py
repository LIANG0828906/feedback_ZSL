"""Deterministic triangle rasterization with per-layer depth buffers."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .math3d import normalize
from .scene import Scene


@dataclass
class PreparedTriangle:
    layer_index: int
    kind: str
    object_id: int
    triangle_index: int
    clip_index: int
    vertices: np.ndarray
    uvs: np.ndarray | None
    normal: np.ndarray
    screen: np.ndarray
    camera_z: np.ndarray
    sort_key: tuple[int, int, int, int, int]


@dataclass
class LayerFragments:
    layer_index: int
    mask: np.ndarray
    world: np.ndarray
    uv: np.ndarray
    normal: np.ndarray
    kind: np.ndarray
    object_id: np.ndarray
    triangle_index: np.ndarray


@dataclass
class WallFragments:
    mask: np.ndarray
    world: np.ndarray
    uv: np.ndarray
    ray_direction: np.ndarray


def _clip_segment(
    a_world: np.ndarray,
    b_world: np.ndarray,
    a_uv: np.ndarray | None,
    b_uv: np.ndarray | None,
    a_z: float,
    b_z: float,
    boundary: float,
    keep_greater: bool,
) -> tuple[np.ndarray, np.ndarray | None, float]:
    denom = b_z - a_z
    t = 0.0 if abs(denom) <= 1.0e-15 else (boundary - a_z) / denom
    world = a_world + (b_world - a_world) * t
    uv = None if a_uv is None or b_uv is None else a_uv + (b_uv - a_uv) * t
    return world, uv, boundary


def _clip_polygon(
    worlds: list[np.ndarray],
    uvs: list[np.ndarray] | None,
    zs: list[float],
    boundary: float,
    keep_greater: bool,
) -> tuple[list[np.ndarray], list[np.ndarray] | None, list[float]]:
    out_worlds: list[np.ndarray] = []
    out_uvs: list[np.ndarray] | None = [] if uvs is not None else None
    out_zs: list[float] = []

    def inside(z: float) -> bool:
        return z >= boundary if keep_greater else z <= boundary

    previous_world = worlds[-1]
    previous_uv = uvs[-1] if uvs is not None else None
    previous_z = zs[-1]
    previous_inside = inside(previous_z)
    for current_world, current_uv, current_z in zip(worlds, uvs if uvs is not None else [None] * len(worlds), zs):
        current_inside = inside(current_z)
        if current_inside != previous_inside:
            world, uv, z = _clip_segment(
                previous_world,
                current_world,
                previous_uv,
                current_uv,
                previous_z,
                current_z,
                boundary,
                keep_greater,
            )
            out_worlds.append(world)
            out_zs.append(z)
            if out_uvs is not None and uv is not None:
                out_uvs.append(uv)
        if current_inside:
            out_worlds.append(current_world)
            out_zs.append(current_z)
            if out_uvs is not None and current_uv is not None:
                out_uvs.append(current_uv)
        previous_world = current_world
        previous_uv = current_uv
        previous_z = current_z
        previous_inside = current_inside
    return out_worlds, out_uvs, out_zs


def _project_vertices(scene: Scene, worlds: np.ndarray, camera_z: np.ndarray) -> np.ndarray:
    relative = worlds - scene.camera.position
    aspect = scene.renderer.width / scene.renderer.height
    x = (relative @ scene.camera.right) / (camera_z * scene.camera.tan_half_fov * aspect)
    y = (relative @ scene.camera.true_up) / (camera_z * scene.camera.tan_half_fov)
    screen_x = (x + 1.0) * 0.5 * scene.renderer.width
    screen_y = (1.0 - y) * 0.5 * scene.renderer.height
    return np.column_stack((screen_x, screen_y))


def prepare_scene(scene: Scene) -> dict[int, list[PreparedTriangle]]:
    prepared: dict[int, list[PreparedTriangle]] = {layer.index: [] for layer in scene.layers}
    kind_rank = {"rib": 0, "board": 1}
    for layer in sorted(scene.layers, key=lambda item: item.index):
        groups = [(rib.rib_id, "rib", rib.mesh) for rib in sorted(layer.ribs, key=lambda item: item.rib_id)]
        groups.extend((board.board_id, "board", board.mesh) for board in sorted(layer.boards, key=lambda item: item.board_id))
        for object_id, kind, mesh in groups:
            for triangle_index, triangle in enumerate(mesh.triangles):
                worlds = [mesh.vertices[int(triangle[0])], mesh.vertices[int(triangle[1])], mesh.vertices[int(triangle[2])]]
                uvs: list[np.ndarray] | None
                if mesh.uvs is not None:
                    uvs = [mesh.uvs[int(triangle[0])], mesh.uvs[int(triangle[1])], mesh.uvs[int(triangle[2])]]
                else:
                    uvs = None
                zs = [float(np.dot(world - scene.camera.position, scene.camera.forward)) for world in worlds]
                worlds, uvs, zs = _clip_polygon(worlds, uvs, zs, scene.camera.near, True)
                if len(worlds) < 3:
                    continue
                worlds, uvs, zs = _clip_polygon(worlds, uvs, zs, scene.camera.far, False)
                if len(worlds) < 3:
                    continue
                for clip_index in range(1, len(worlds) - 1):
                    tri_worlds = np.asarray([worlds[0], worlds[clip_index], worlds[clip_index + 1]], dtype=np.float64)
                    tri_uvs = None
                    if uvs is not None:
                        tri_uvs = np.asarray([uvs[0], uvs[clip_index], uvs[clip_index + 1]], dtype=np.float64)
                    tri_z = np.asarray([zs[0], zs[clip_index], zs[clip_index + 1]], dtype=np.float64)
                    if np.any(tri_z <= 0.0):
                        continue
                    screen = _project_vertices(scene, tri_worlds, tri_z)
                    prepared[layer.index].append(
                        PreparedTriangle(
                            layer_index=layer.index,
                            kind=kind,
                            object_id=object_id,
                            triangle_index=triangle_index,
                            clip_index=clip_index,
                            vertices=tri_worlds,
                            uvs=tri_uvs,
                            normal=mesh.normals[triangle_index].copy(),
                            screen=screen,
                            camera_z=tri_z,
                            sort_key=(layer.index, kind_rank[kind], object_id, triangle_index, clip_index),
                        )
                    )
        prepared[layer.index].sort(key=lambda item: item.sort_key)
    return prepared


def _edge(ax: np.ndarray, ay: np.ndarray, bx: np.ndarray, by: np.ndarray, px: np.ndarray, py: np.ndarray) -> np.ndarray:
    return (bx - ax) * (py - ay) - (by - ay) * (px - ax)


def rasterize_layer_sample(
    prepared: list[PreparedTriangle],
    *,
    layer_index: int,
    sample_x: float,
    sample_y: float,
    block_x: int,
    block_y: int,
    block_width: int,
    block_height: int,
) -> LayerFragments:
    depth = np.full((block_height, block_width), np.inf, dtype=np.float64)
    triangle_buffer = np.full((block_height, block_width), -1, dtype=np.int64)
    bary_buffer = np.zeros((block_height, block_width, 3), dtype=np.float64)
    xs = np.arange(block_x, block_x + block_width, dtype=np.float64) + sample_x
    ys = np.arange(block_y, block_y + block_height, dtype=np.float64) + sample_y

    for triangle_id, triangle in enumerate(prepared):
        x = triangle.screen[:, 0]
        y = triangle.screen[:, 1]
        min_x = float(np.min(x))
        max_x = float(np.max(x))
        min_y = float(np.min(y))
        max_y = float(np.max(y))
        ix0 = max(0, int(np.floor(min_x - block_x - sample_x)) - 1)
        ix1 = min(block_width - 1, int(np.ceil(max_x - block_x - sample_x)) + 1)
        iy0 = max(0, int(np.floor(min_y - block_y - sample_y)) - 1)
        iy1 = min(block_height - 1, int(np.ceil(max_y - block_y - sample_y)) + 1)
        if ix0 > ix1 or iy0 > iy1:
            continue
        xx, yy = np.meshgrid(xs[ix0 : ix1 + 1], ys[iy0 : iy1 + 1])
        x0, y0 = x[0], y[0]
        x1, y1 = x[1], y[1]
        x2, y2 = x[2], y[2]
        denominator = _edge(np.array(x0), np.array(y0), np.array(x1), np.array(y1), np.array(x2), np.array(y2))
        if abs(float(denominator)) <= 1.0e-14:
            continue
        b0 = _edge(np.array(x1), np.array(y1), np.array(x2), np.array(y2), xx, yy) / denominator
        b1 = _edge(np.array(x2), np.array(y2), np.array(x0), np.array(y0), xx, yy) / denominator
        b2 = 1.0 - b0 - b1
        inside = (b0 >= -1.0e-9) & (b1 >= -1.0e-9) & (b2 >= -1.0e-9)
        if not np.any(inside):
            continue
        z0, z1, z2 = triangle.camera_z
        with np.errstate(divide="ignore", invalid="ignore"):
            inv_z = b0 / z0 + b1 / z1 + b2 / z2
            z = 1.0 / inv_z
        valid = inside & np.isfinite(z) & (z > 0.0)
        if not np.any(valid):
            continue
        current_depth = depth[iy0 : iy1 + 1, ix0 : ix1 + 1]
        better = valid & (z < current_depth)
        if not np.any(better):
            continue
        current_depth[better] = z[better]
        triangle_buffer[iy0 : iy1 + 1, ix0 : ix1 + 1][better] = triangle_id
        bary_view = bary_buffer[iy0 : iy1 + 1, ix0 : ix1 + 1]
        bary_view[better, 0] = b0[better]
        bary_view[better, 1] = b1[better]
        bary_view[better, 2] = b2[better]

    mask = triangle_buffer >= 0
    world = np.zeros((block_height, block_width, 3), dtype=np.float64)
    uv = np.zeros((block_height, block_width, 2), dtype=np.float64)
    normal = np.zeros((block_height, block_width, 3), dtype=np.float64)
    kind = np.zeros((block_height, block_width), dtype=np.uint8)
    object_id = np.full((block_height, block_width), -1, dtype=np.int32)
    triangle_source = np.full((block_height, block_width), -1, dtype=np.int32)
    if np.any(mask):
        rows, cols = np.nonzero(mask)
        ids = triangle_buffer[rows, cols]
        bary = bary_buffer[rows, cols]
        for local_triangle_id in np.unique(ids):
            selection = ids == local_triangle_id
            triangle = prepared[int(local_triangle_id)]
            b = bary[selection]
            z = triangle.camera_z
            inv = b / z[None, :]
            weights = inv / np.sum(inv, axis=1, keepdims=True)
            rr = rows[selection]
            cc = cols[selection]
            world[rr, cc] = weights @ triangle.vertices
            if triangle.uvs is not None:
                uv[rr, cc] = weights @ triangle.uvs
            normal[rr, cc] = triangle.normal
            kind[rr, cc] = 1 if triangle.kind == "rib" else 2
            object_id[rr, cc] = triangle.object_id
            triangle_source[rr, cc] = triangle.triangle_index
    return LayerFragments(
        layer_index=layer_index,
        mask=mask,
        world=world,
        uv=uv,
        normal=normal,
        kind=kind,
        object_id=object_id,
        triangle_index=triangle_source,
    )


def ray_directions(scene: Scene, sample_x: float, sample_y: float, block_x: int, block_y: int, width: int, height: int) -> np.ndarray:
    xs = (np.arange(block_x, block_x + width, dtype=np.float64) + sample_x) * (2.0 / scene.renderer.width) - 1.0
    ys = 1.0 - (np.arange(block_y, block_y + height, dtype=np.float64) + sample_y) * (2.0 / scene.renderer.height)
    xx, yy = np.meshgrid(xs, ys)
    aspect = scene.renderer.width / scene.renderer.height
    directions = (
        scene.camera.forward[None, None, :]
        + (xx * scene.camera.tan_half_fov * aspect)[..., None] * scene.camera.right[None, None, :]
        + (yy * scene.camera.tan_half_fov)[..., None] * scene.camera.true_up[None, None, :]
    )
    lengths = np.linalg.norm(directions, axis=2)
    return directions / lengths[..., None]


def rasterize_wall_sample(
    scene: Scene,
    *,
    sample_x: float,
    sample_y: float,
    block_x: int,
    block_y: int,
    width: int,
    height: int,
) -> WallFragments:
    directions = ray_directions(scene, sample_x, sample_y, block_x, block_y, width, height)
    wall = scene.wall
    denominator = directions @ wall.normal
    numerator = float(np.dot(wall.point - scene.camera.position, wall.normal))
    with np.errstate(divide="ignore", invalid="ignore"):
        t = numerator / denominator
    hit = np.isfinite(t) & (t > scene.camera.near) & (t < scene.camera.far) & (np.abs(denominator) > 1.0e-12)
    world = scene.camera.position[None, None, :] + directions * t[..., None]
    relative = world - wall.point[None, None, :]
    local_u = np.sum(relative * wall.u_axis[None, None, :], axis=2) / wall.half_size[0]
    local_v = np.sum(relative * wall.v_axis[None, None, :], axis=2) / wall.half_size[1]
    hit &= (local_u >= -1.0) & (local_u <= 1.0) & (local_v >= -1.0) & (local_v <= 1.0)
    uv = np.stack(((local_u + 1.0) * 0.5, (local_v + 1.0) * 0.5), axis=2)
    return WallFragments(mask=hit, world=world, uv=uv, ray_direction=directions)
