"""Deterministic tile rasterization and shading."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .optics import (
    CausticSamples,
    FilmTables,
    build_caustic_samples,
    build_film_tables,
    caustic_point_contributions,
    film_linear_rgb,
)
from .scene import SceneBundle


@dataclass(frozen=True)
class BlockSpec:
    block_id: str
    camera_id: str
    camera_index: int
    block_index: int
    x: int
    y: int
    width: int
    height: int


@dataclass
class TileResult:
    rgba: np.ndarray
    face_id: np.ndarray
    thickness_sum: float
    thickness_count: int
    visible_face_count: int


@dataclass
class RenderContext:
    bundle: SceneBundle
    tables: FilmTables
    caustics: dict[str, CausticSamples]
    lights: list[dict[str, Any]]
    face_order: np.ndarray
    v0: np.ndarray
    edge1: np.ndarray
    edge2: np.ndarray
    face_ids: np.ndarray
    face_normals: np.ndarray
    leaf_y: float
    leaf_bounds: np.ndarray
    block_w: int
    block_h: int
    exposure: float
    caustic_gain: float


def build_context(bundle: SceneBundle) -> RenderContext:
    scene = bundle.scene
    order = np.argsort(bundle.face_ids, kind="stable")
    triangles = bundle.triangles[order]
    face_ids = bundle.face_ids[order]
    v0 = bundle.vertices[triangles[:, 0]]
    edge1 = bundle.vertices[triangles[:, 1]] - v0
    edge2 = bundle.vertices[triangles[:, 2]] - v0
    normals = np.cross(edge1, edge2)
    normals /= np.maximum(np.linalg.norm(normals, axis=1, keepdims=True), 1.0e-12)
    tables = build_film_tables(bundle)
    caustics = build_caustic_samples(bundle, tables)
    block_w, block_h = (int(v) for v in scene["render"]["block_size"])
    return RenderContext(
        bundle=bundle,
        tables=tables,
        caustics=caustics,
        lights=sorted(scene["lights"], key=lambda item: item["id"]),
        face_order=order,
        v0=v0,
        edge1=edge1,
        edge2=edge2,
        face_ids=face_ids,
        face_normals=normals,
        leaf_y=float(scene["leaf"]["y"]),
        leaf_bounds=np.asarray(scene["leaf"]["bounds"], dtype=np.float64),
        block_w=block_w,
        block_h=block_h,
        exposure=float(scene["render"]["exposure"]),
        caustic_gain=float(scene["leaf"]["caustic_gain"]),
    )


def enumerate_blocks(scene: dict[str, Any]) -> list[BlockSpec]:
    block_w, block_h = (int(v) for v in scene["render"]["block_size"])
    blocks: list[BlockSpec] = []
    for camera_index, camera in enumerate(scene["cameras"]):
        width = int(camera["width"])
        height = int(camera["height"])
        block_index = 0
        for y in range(0, height, block_h):
            for x in range(0, width, block_w):
                w = min(block_w, width - x)
                h = min(block_h, height - y)
                blocks.append(
                    BlockSpec(
                        block_id=f"{camera['id']}/b{block_index:04d}",
                        camera_id=str(camera["id"]),
                        camera_index=camera_index,
                        block_index=block_index,
                        x=x,
                        y=y,
                        width=w,
                        height=h,
                    )
                )
                block_index += 1
    return blocks


def camera_basis(camera: dict[str, Any]) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    origin = np.asarray(camera["position"], dtype=np.float64)
    target = np.asarray(camera["target"], dtype=np.float64)
    up = np.asarray(camera["up"], dtype=np.float64)
    forward = target - origin
    forward /= np.linalg.norm(forward)
    right = np.cross(forward, up)
    right /= np.linalg.norm(right)
    true_up = np.cross(right, forward)
    return origin, forward, right, true_up


def camera_rays(camera: dict[str, Any], xs: np.ndarray, ys: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    origin, forward, right, true_up = camera_basis(camera)
    width = int(camera["width"])
    height = int(camera["height"])
    aspect = width / height
    half_height = math.tan(math.radians(float(camera["fov_y_deg"])) * 0.5)
    ndc_x = ((xs.astype(np.float64) + 0.5) / width * 2.0 - 1.0) * half_height * aspect
    ndc_y = (1.0 - (ys.astype(np.float64) + 0.5) / height * 2.0) * half_height
    directions = forward[None, :] + ndc_x[:, None] * right[None, :] + ndc_y[:, None] * true_up[None, :]
    return origin, directions


def _intersect_elytron(
    context: RenderContext,
    origin: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    pixel_count = directions.shape[0]
    best_t = np.full((pixel_count,), np.inf, dtype=np.float64)
    best_face = np.full((pixel_count,), -1, dtype=np.int64)
    best_u = np.zeros((pixel_count,), dtype=np.float64)
    best_v = np.zeros((pixel_count,), dtype=np.float64)
    for start in range(0, pixel_count, 384):
        stop = min(start + 384, pixel_count)
        rays = directions[start:stop]
        pvec = np.cross(rays[:, None, :], context.edge2[None, :, :])
        det = np.einsum("kj,ikj->ik", context.edge1, pvec)
        valid_det = np.abs(det) > 1.0e-11
        inv_det = np.where(valid_det, 1.0 / np.where(valid_det, det, 1.0), 0.0)
        tvec = origin[None, None, :] - context.v0[None, :, :]
        u = np.einsum("ikj,ikj->ik", tvec, pvec) * inv_det
        qvec = np.cross(tvec, context.edge1[None, :, :])
        v = np.einsum("ikj,ikj->ik", rays[:, None, :], qvec) * inv_det
        t = np.einsum("kj,ikj->ik", context.edge2, qvec) * inv_det
        hit = (
            valid_det
            & (u >= -1.0e-9)
            & (v >= -1.0e-9)
            & (u + v <= 1.0 + 1.0e-9)
            & (t > 1.0e-7)
        )
        t = np.where(hit, t, np.inf)
        local = np.argmin(t, axis=1)
        local_t = t[np.arange(stop - start), local]
        found = np.isfinite(local_t)
        best_t[start:stop] = local_t
        best_face[start:stop] = np.where(found, local, -1)
        best_u[start:stop] = np.where(found, u[np.arange(stop - start), local], 0.0)
        best_v[start:stop] = np.where(found, v[np.arange(stop - start), local], 0.0)
    return best_t, best_face, best_u, best_v


def _tone_map(linear: np.ndarray, exposure: float, saturation_boost: float = 1.0) -> np.ndarray:
    value = np.maximum(linear, 0.0) * exposure
    value = value / (1.0 + value)
    srgb = np.where(
        value <= 0.0031308,
        12.92 * value,
        1.055 * np.power(value, 1.0 / 2.4) - 0.055,
    )
    if saturation_boost != 1.0:
        display_luminance = srgb @ np.array([0.2126, 0.7152, 0.0722])
        srgb = display_luminance[:, None] + (
            srgb - display_luminance[:, None]
        ) * saturation_boost
    return np.floor(np.clip(srgb, 0.0, 1.0) * 255.0 + 0.5).astype(np.uint8)


def _shade_surface_points(
    context: RenderContext,
    face_ids: np.ndarray,
    bary: np.ndarray,
    points: np.ndarray,
    view_dirs: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    film = context.bundle.scene["film"]
    normals = context.tables.normals(face_ids, bary)
    view_dirs = view_dirs / np.maximum(
        np.linalg.norm(view_dirs, axis=1, keepdims=True), 1.0e-12
    )
    cosine_view = np.clip(np.einsum("ij,ij->i", normals, view_dirs), 0.04, 1.0)
    thickness = context.tables.thickness(face_ids, bary, points, film)
    film_rgb = film_linear_rgb(thickness, cosine_view, film)
    peak = np.maximum(np.max(film_rgb, axis=1, keepdims=True), 1.0e-12)
    film_tint = film_rgb / peak
    film_tint = film_tint * film_tint
    film_tint /= np.maximum(np.max(film_tint, axis=1, keepdims=True), 1.0e-12)
    film_luminance = film_rgb @ np.array([0.2126, 0.7152, 0.0722])
    fresnel = 0.04 + 0.96 * np.power(1.0 - cosine_view, 5.0)
    linear = film_tint * (0.36 + 0.78 * film_luminance[:, None])
    linear *= 0.86 + 0.32 * fresnel[:, None]
    for light in context.lights:
        source = np.asarray(light["position"], dtype=np.float64)
        to_light = source[None, :] - points
        distance = np.linalg.norm(to_light, axis=1)
        light_dir = to_light / np.maximum(distance[:, None], 1.0e-12)
        half = light_dir + view_dirs
        half /= np.maximum(np.linalg.norm(half, axis=1, keepdims=True), 1.0e-12)
        lobe = np.maximum(np.einsum("ij,ij->i", normals, half), 0.0) ** 64
        intensity = float(light["intensity"]) / (1.0 + 0.18 * distance * distance)
        specular = lobe * intensity * (0.25 + 0.75 * fresnel)
        light_tint = 0.93 * film_tint + 0.07
        linear += specular[:, None] * light_tint
    return thickness, linear


def _leaf_linear(context: RenderContext, points: np.ndarray) -> np.ndarray:
    scene = context.bundle.scene
    base = np.asarray(scene["leaf"]["base_color"], dtype=np.float64)
    x = points[:, 0]
    z = points[:, 2]
    variation = 0.011 * np.sin(8.5 * x + 1.2) * np.cos(7.5 * z - 0.6)
    linear = np.empty((points.shape[0], 3), dtype=np.float64)
    linear[:, 0] = base[0] + variation
    linear[:, 1] = base[1] + 1.4 * variation
    linear[:, 2] = base[2] + 0.6 * variation
    contributions = caustic_point_contributions(
        context.caustics, points, context.caustic_gain
    )
    for source_id in sorted(contributions):
        linear += contributions[source_id]
    return linear


def render_block(context: RenderContext, camera: dict[str, Any], spec: BlockSpec) -> TileResult:
    width = spec.width
    height = spec.height
    yy, xx = np.mgrid[0:height, 0:width]
    xs = xx.reshape(-1) + spec.x
    ys = yy.reshape(-1) + spec.y
    origin, directions = camera_rays(camera, xs, ys)
    _, best_face, best_u, best_v = _intersect_elytron(context, origin, directions)
    pixel_count = xs.shape[0]
    rgba = np.zeros((height, width, 4), dtype=np.uint8)
    face_map = np.zeros((height, width), dtype=np.int32)
    thickness_sum = 0.0
    thickness_count = 0
    object_mask = best_face >= 0

    if np.any(object_mask):
        local_faces = best_face[object_mask]
        u = best_u[object_mask]
        v = best_v[object_mask]
        bary = np.stack(
            [1.0 - u - v, u, v],
            axis=1,
        )
        fids = context.face_ids[local_faces]
        triangles = context.bundle.triangles[context.face_order[local_faces]]
        p0 = context.bundle.vertices[triangles[:, 0]]
        p1 = context.bundle.vertices[triangles[:, 1]]
        p2 = context.bundle.vertices[triangles[:, 2]]
        points = (
            bary[:, 0, None] * p0
            + bary[:, 1, None] * p1
            + bary[:, 2, None] * p2
        )
        view_dirs = origin[None, :] - points
        thickness, linear = _shade_surface_points(
            context, fids, bary, points, view_dirs
        )
        display = _tone_map(linear, context.exposure, saturation_boost=1.48)
        rgba_flat = rgba.reshape(-1, 4)
        rgba_flat[object_mask, :3] = display
        rgba_flat[object_mask, 3] = 255
        face_flat = face_map.reshape(-1)
        face_flat[object_mask] = fids
        thickness_sum = float(np.sum(thickness))
        thickness_count = int(thickness.shape[0])

    background_mask = ~object_mask
    if np.any(background_mask):
        t_leaf = (context.leaf_y - origin[1]) / np.where(
            np.abs(directions[background_mask, 1]) > 1.0e-12,
            directions[background_mask, 1],
            -1.0e-12,
        )
        leaf_points = (
            origin[None, :]
            + t_leaf[:, None] * directions[background_mask]
        )
        inside_leaf = (
            (t_leaf > 1.0e-7)
            & (leaf_points[:, 0] >= context.leaf_bounds[0])
            & (leaf_points[:, 0] <= context.leaf_bounds[1])
            & (leaf_points[:, 2] >= context.leaf_bounds[2])
            & (leaf_points[:, 2] <= context.leaf_bounds[3])
        )
        background_indices = np.flatnonzero(background_mask)
        rgba_flat = rgba.reshape(-1, 4)
        if np.any(inside_leaf):
            selected = background_indices[inside_leaf]
            points = leaf_points[inside_leaf]
            linear = _leaf_linear(context, points)
            display = _tone_map(linear, context.exposure, saturation_boost=1.45)
            rgba_flat[selected, :3] = display
        outside = background_indices[~inside_leaf]
        if outside.size:
            rgba_flat[outside, :3] = np.array([4, 8, 6], dtype=np.uint8)
        rgba_flat[background_indices, 3] = 255

    unique_faces = np.unique(face_map)
    unique_faces = unique_faces[unique_faces > 0]
    return TileResult(
        rgba=rgba,
        face_id=face_map,
        thickness_sum=thickness_sum,
        thickness_count=thickness_count,
        visible_face_count=int(unique_faces.size),
    )


def hsv_hue(rgb: np.ndarray) -> tuple[float, float]:
    red, green, blue = (float(v) for v in rgb)
    maximum = max(red, green, blue)
    minimum = min(red, green, blue)
    delta = maximum - minimum
    if delta <= 1.0e-12:
        return 0.0, 0.0
    if maximum == red:
        hue = ((green - blue) / delta) % 6.0
    elif maximum == green:
        hue = (blue - red) / delta + 2.0
    else:
        hue = (red - green) / delta + 4.0
    hue *= 60.0
    saturation = 0.0 if maximum <= 1.0e-12 else delta / maximum
    return hue, saturation


def _circular_hue_difference(a: float, b: float) -> float:
    difference = abs(a - b) % 360.0
    return min(difference, 360.0 - difference)


def hue_continuity(context: RenderContext) -> dict[str, Any]:
    scene = context.bundle.scene
    film = scene["film"]
    threshold = float(film["hue_jump_threshold_deg"])
    saturation_gate = 0.10
    centers = context.bundle.vertices[context.bundle.triangles].mean(axis=1)
    # Keep the demonstrated sequence on central, near-normal facets that stay
    # visible for the complete orbit. This avoids silhouette coverage changes
    # being mistaken for an optical hue discontinuity.
    score = centers[:, 1] - 0.55 * np.abs(centers[:, 0]) - 0.25 * np.abs(centers[:, 2])
    candidate_order = np.argsort(-score, kind="stable")
    selected: list[int] = []
    for ordinal in candidate_order:
        face_id = int(context.bundle.face_ids[ordinal])
        if all(
            np.linalg.norm(centers[ordinal] - centers[other]) > 0.08
            for other in selected
        ):
            selected.append(int(ordinal))
        if len(selected) == 4:
            break

    samples: list[dict[str, Any]] = []
    previous_hue: dict[int, tuple[float, float]] = {}
    max_jump = 0.0
    for camera_index, camera in enumerate(scene["cameras"]):
        origin = np.asarray(camera["position"], dtype=np.float64)
        records = []
        for ordinal in selected:
            face_id = int(context.bundle.face_ids[ordinal])
            triangle = context.bundle.triangles[ordinal]
            point = centers[ordinal]
            bary = np.array([[1.0 / 3.0, 1.0 / 3.0, 1.0 / 3.0]], dtype=np.float64)
            _, linear = _shade_surface_points(
                context,
                np.array([face_id], dtype=np.int64),
                bary,
                point[None, :],
                (origin - point)[None, :],
            )
            rgb = _tone_map(linear, context.exposure, saturation_boost=1.48)[0].astype(np.float64) / 255.0
            hue, saturation = hsv_hue(rgb)
            jump = None
            if saturation >= saturation_gate:
                if face_id in previous_hue:
                    previous_value, previous_saturation = previous_hue[face_id]
                    if previous_saturation >= saturation_gate:
                        jump = _circular_hue_difference(previous_value, hue)
                        max_jump = max(max_jump, jump)
                previous_hue[face_id] = (hue, saturation)
            records.append(
                {
                    "face_id": face_id,
                    "hue_deg": round(hue, 9),
                    "saturation": round(saturation, 9),
                    "jump_from_previous_deg": None if jump is None else round(jump, 9),
                }
            )
        samples.append({"camera_id": str(camera["id"]), "faces": records})
    return {
        "threshold_deg": threshold,
        "saturation_gate": saturation_gate,
        "max_adjacent_jump_deg": round(max_jump, 9),
        "sample_face_ids": [int(context.bundle.face_ids[i]) for i in selected],
        "samples": samples,
        "pass": bool(max_jump <= threshold + 1.0e-9),
    }
