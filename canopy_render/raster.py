from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .camera import Camera
from .validation import LayerData


@dataclass(frozen=True)
class LayerGeometry:
    vertices: np.ndarray
    uvs: np.ndarray
    normals: np.ndarray
    triangles: np.ndarray
    triangle_fold_ids: np.ndarray


@dataclass
class LayerHits:
    mask: np.ndarray
    depth: np.ndarray
    triangle_index: np.ndarray
    barycentric: np.ndarray
    point: np.ndarray
    uv: np.ndarray
    normal: np.ndarray


def build_layer_geometry(layer: LayerData) -> LayerGeometry:
    all_vertices: list[np.ndarray] = []
    all_uvs: list[np.ndarray] = []
    all_normals: list[np.ndarray] = []
    all_triangles: list[np.ndarray] = []
    all_fold_ids: list[np.ndarray] = []
    vertex_offset = 0

    for fold in layer.folds:
        vertices = fold.vertices
        uvs = fold.uvs
        triangles = fold.triangles
        normals = np.zeros_like(vertices, dtype=np.float64)
        for index in range(len(triangles)):
            ia, ib, ic = (int(v) for v in triangles[index])
            a, b, c = vertices[ia], vertices[ib], vertices[ic]
            face = np.cross(b - a, c - a)
            area_twice = float(np.linalg.norm(face))
            if area_twice <= 1e-14:
                continue
            unit = face / area_twice
            normals[ia] += unit * area_twice * 0.5
            normals[ib] += unit * area_twice * 0.5
            normals[ic] += unit * area_twice * 0.5
        lengths = np.linalg.norm(normals, axis=1)
        good = lengths > 1e-12
        normals[good] /= lengths[good, None]
        # Degenerate normal fallbacks are unreachable for validated meshes, but
        # keep the array finite if a mesh has isolated unreferenced vertices.
        normals[~good] = np.array([0.0, 1.0, 0.0])

        all_vertices.append(vertices)
        all_uvs.append(uvs)
        all_normals.append(normals)
        all_triangles.append(triangles + vertex_offset)
        all_fold_ids.append(
            np.full(len(triangles), fold.fold_id, dtype=np.int64)
        )
        vertex_offset += len(vertices)

    return LayerGeometry(
        vertices=np.concatenate(all_vertices, axis=0),
        uvs=np.concatenate(all_uvs, axis=0),
        normals=np.concatenate(all_normals, axis=0),
        triangles=np.concatenate(all_triangles, axis=0),
        triangle_fold_ids=np.concatenate(all_fold_ids, axis=0),
    )


def _triangle_bbox(
    camera: Camera,
    screen_xy: np.ndarray,
    x0: int,
    y0: int,
    width: int,
    height: int,
) -> tuple[int, int, int, int] | None:
    min_x = int(np.floor(float(np.min(screen_xy[:, 0])))) - 1
    max_x = int(np.ceil(float(np.max(screen_xy[:, 0])))) + 1
    min_y = int(np.floor(float(np.min(screen_xy[:, 1])))) - 1
    max_y = int(np.ceil(float(np.max(screen_xy[:, 1])))) + 1
    min_x = max(min_x, x0)
    min_y = max(min_y, y0)
    max_x = min(max_x, x0 + width - 1)
    max_y = min(max_y, y0 + height - 1)
    if min_x > max_x or min_y > max_y:
        return None
    return min_x, min_y, max_x + 1, max_y + 1


def rasterize_layer(
    camera: Camera,
    geometry: LayerGeometry,
    rays: np.ndarray,
    x0: int,
    y0: int,
) -> LayerHits:
    height, width = rays.shape[:2]
    depth = np.full((height, width), np.inf, dtype=np.float64)
    triangle_index = np.full((height, width), -1, dtype=np.int64)
    barycentric = np.zeros((height, width, 3), dtype=np.float64)

    screen_x, screen_y, view_depth = camera.project_points(geometry.vertices)
    screen_xy = np.stack((screen_x, screen_y), axis=1)

    for tri_index in range(len(geometry.triangles)):
        ia, ib, ic = (int(v) for v in geometry.triangles[tri_index])
        if min(view_depth[ia], view_depth[ib], view_depth[ic]) <= camera.near:
            continue
        bbox = _triangle_bbox(
            camera,
            screen_xy[[ia, ib, ic]],
            x0,
            y0,
            width,
            height,
        )
        if bbox is None:
            continue
        bx0, by0, bx1, by1 = bbox
        local_rays = rays[by0 - y0 : by1 - y0, bx0 - x0 : bx1 - x0, :]

        a = geometry.vertices[ia]
        b = geometry.vertices[ib]
        c = geometry.vertices[ic]
        edge1 = b - a
        edge2 = c - a
        pvec = np.cross(local_rays, edge2)
        determinant = np.einsum("hwc,c->hw", pvec, edge1)
        usable = np.abs(determinant) > 1e-12
        if not np.any(usable):
            continue

        inverse = np.zeros_like(determinant)
        inverse[usable] = 1.0 / determinant[usable]
        tvec = camera.position[None, None, :] - a[None, None, :]
        u = np.einsum("hwc,hwc->hw", tvec, pvec) * inverse
        qvec = np.cross(tvec, edge1[None, None, :])
        v = np.einsum("hwc,hwc->hw", local_rays, qvec) * inverse
        t = np.einsum("c,hwc->hw", edge2, qvec) * inverse
        inside = (
            usable
            & (u >= -1e-10)
            & (v >= -1e-10)
            & (u + v <= 1.0 + 1e-10)
            & (t > camera.near)
            & (t < camera.far)
        )
        if not np.any(inside):
            continue

        depth_view = depth[by0 - y0 : by1 - y0, bx0 - x0 : bx1 - x0]
        closer = inside & (t < depth_view - 1e-9)
        if not np.any(closer):
            continue
        depth_view[closer] = t[closer]
        triangle_view = triangle_index[
            by0 - y0 : by1 - y0, bx0 - x0 : bx1 - x0
        ]
        triangle_view[closer] = tri_index
        bary_view = barycentric[by0 - y0 : by1 - y0, bx0 - x0 : bx1 - x0, :]
        bary_view[closer, 0] = 1.0 - u[closer] - v[closer]
        bary_view[closer, 1] = u[closer]
        bary_view[closer, 2] = v[closer]

    mask = triangle_index >= 0
    valid_triangles = triangle_index[mask]
    valid_bary = barycentric[mask]
    tri_vertices = geometry.triangles[valid_triangles]
    points = (
        valid_bary[:, 0, None] * geometry.vertices[tri_vertices[:, 0]]
    )
    points += (
        valid_bary[:, 1, None] * geometry.vertices[tri_vertices[:, 1]]
    )
    points += (
        valid_bary[:, 2, None] * geometry.vertices[tri_vertices[:, 2]]
    )
    uv = (
        valid_bary[:, 0, None] * geometry.uvs[tri_vertices[:, 0]]
        + valid_bary[:, 1, None] * geometry.uvs[tri_vertices[:, 1]]
        + valid_bary[:, 2, None] * geometry.uvs[tri_vertices[:, 2]]
    )
    normal = (
        valid_bary[:, 0, None] * geometry.normals[tri_vertices[:, 0]]
        + valid_bary[:, 1, None] * geometry.normals[tri_vertices[:, 1]]
        + valid_bary[:, 2, None] * geometry.normals[tri_vertices[:, 2]]
    )
    normal_lengths = np.linalg.norm(normal, axis=1)
    normal_good = normal_lengths > 1e-12
    normal[normal_good] /= normal_lengths[normal_good, None]
    normal[~normal_good] = np.array([0.0, 1.0, 0.0])

    point_image = np.zeros((height, width, 3), dtype=np.float64)
    uv_image = np.zeros((height, width, 2), dtype=np.float64)
    normal_image = np.zeros((height, width, 3), dtype=np.float64)
    point_image[mask] = points
    uv_image[mask] = uv
    normal_image[mask] = normal
    return LayerHits(
        mask=mask,
        depth=depth,
        triangle_index=triangle_index,
        barycentric=barycentric,
        point=point_image,
        uv=uv_image,
        normal=normal_image,
    )

