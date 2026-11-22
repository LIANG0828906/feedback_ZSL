"""Vectorized deterministic ray casting and shading."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Literal

import numpy as np

from .model import Camera, Scene

ShadingQuality = Literal["preview", "exact"]


@dataclass
class ShadeResult:
    rgb: np.ndarray
    depth: np.ndarray
    object_id: np.ndarray
    normals: np.ndarray


def _as_float32(value: np.ndarray) -> np.ndarray:
    return np.clip(value, 0.0, 1.0).astype(np.float32, copy=False)


def _vector_for(origins: np.ndarray, value: Any) -> np.ndarray:
    arr = np.asarray(value, dtype=np.float64)
    return arr.reshape((1,) * (origins.ndim - 1) + (3,))


def make_rays(
    camera: Camera,
    width: int,
    height: int,
    *,
    x0: int = 0,
    y0: int = 0,
    region_width: int | None = None,
    region_height: int | None = None,
) -> tuple[np.ndarray, np.ndarray]:
    rw = width if region_width is None else int(region_width)
    rh = height if region_height is None else int(region_height)
    xs = np.arange(x0, x0 + rw, dtype=np.float64)
    ys = np.arange(y0, y0 + rh, dtype=np.float64)
    px = ((xs + 0.5) / float(width)) * 2.0 - 1.0
    py = 1.0 - ((ys + 0.5) / float(height)) * 2.0
    tan_half = math.tan(math.radians(camera.fov_y) * 0.5)
    aspect = float(width) / float(height)
    forward = camera.forward
    right = camera.right
    up = camera.true_up
    x_component = px[None, :, None] * (tan_half * aspect)
    y_component = py[:, None, None] * tan_half
    directions = forward[None, None, :] + right[None, None, :] * x_component
    directions = directions + up[None, None, :] * y_component
    lengths = np.sqrt(np.sum(directions * directions, axis=-1, keepdims=True))
    directions = directions / lengths
    origins = np.broadcast_to(
        np.asarray(camera.position, dtype=np.float64)[None, None, :],
        directions.shape,
    )
    return origins, directions


def _background(
    scene: Scene,
    camera: Camera,
    width: int,
    height: int,
    x0: int,
    y0: int,
    region_width: int | None = None,
    region_height: int | None = None,
) -> np.ndarray:
    rw = width if region_width is None else int(region_width)
    rh = height if region_height is None else int(region_height)
    ys = np.arange(y0, y0 + rh, dtype=np.float64)
    v = np.clip((ys + 0.5) / float(height), 0.0, 1.0)[:, None, None]
    top = np.asarray(scene.background["top"], dtype=np.float64)[None, None, :]
    bottom = np.asarray(scene.background["bottom"], dtype=np.float64)[None, None, :]
    gradient = top * (1.0 - v) + bottom * v
    xs = np.arange(x0, x0 + rw, dtype=np.float64)
    u = ((xs + 0.5) / float(width)) * 2.0 - 1.0
    glow = np.exp(-(u[None, :, None] ** 2) * 2.2) * 0.035
    gradient = gradient + glow
    return np.broadcast_to(gradient, (rh, rw, 3)).copy()


def _sphere_intersection(
    origins: np.ndarray,
    directions: np.ndarray,
    center: list[float],
    radius: float,
    near: float,
    far: float,
) -> tuple[np.ndarray, np.ndarray]:
    center_array = _vector_for(origins, center)
    oc = origins - center_array
    b = np.sum(oc * directions, axis=-1)
    c = np.sum(oc * oc, axis=-1) - radius * radius
    disc = b * b - c
    valid_disc = disc >= 0.0
    sqrt_disc = np.sqrt(np.maximum(disc, 0.0))
    t0 = -b - sqrt_disc
    t1 = -b + sqrt_disc
    t = np.where(t0 > near, t0, t1)
    hit = valid_disc & (t > near) & (t < far)
    point = origins + directions * t[..., None]
    normal = (point - center_array) / radius
    return np.where(hit, t, np.inf), normal


def _plane_intersection(
    origins: np.ndarray,
    directions: np.ndarray,
    point_on_plane: list[float],
    normal: list[float],
    near: float,
    far: float,
) -> tuple[np.ndarray, np.ndarray]:
    plane_point = _vector_for(origins, point_on_plane)
    plane_normal = _vector_for(origins, normal)
    denom = np.sum(directions * plane_normal, axis=-1)
    with np.errstate(divide="ignore", invalid="ignore", over="ignore"):
        numer = np.sum((plane_point - origins) * plane_normal, axis=-1)
        t = numer / denom
    hit = (np.abs(denom) > 1e-10) & (t > near) & (t < far)
    normal_out = np.broadcast_to(plane_normal, origins.shape).copy()
    return np.where(hit, t, np.inf), normal_out


def _box_intersection(
    origins: np.ndarray,
    directions: np.ndarray,
    center: list[float],
    half_extents: list[float],
    near: float,
    far: float,
) -> tuple[np.ndarray, np.ndarray]:
    center_array = _vector_for(origins, center)[0]
    half_array = _vector_for(origins, half_extents)[0]
    lo = center_array - half_array
    hi = center_array + half_array
    safe_dirs = np.where(
        np.abs(directions) < 1e-12,
        np.copysign(1e-12, directions + 1e-30),
        directions,
    )
    lo_view = _vector_for(origins, lo)
    hi_view = _vector_for(origins, hi)
    t1 = (lo_view - origins) / safe_dirs
    t2 = (hi_view - origins) / safe_dirs
    t_near_axis = np.minimum(t1, t2)
    t_far_axis = np.maximum(t1, t2)
    entry = np.max(t_near_axis, axis=-1)
    exit_ = np.min(t_far_axis, axis=-1)
    hit = (exit_ >= entry) & (entry > near) & (entry < far)
    axis = np.argmax(t_near_axis, axis=-1)
    ray_axis = np.take_along_axis(directions, axis[..., None], axis=-1)[..., 0]
    sign = -np.sign(ray_axis)
    sign = np.where(sign == 0.0, 1.0, sign)
    normals = np.zeros_like(directions, dtype=np.float64)
    np.put_along_axis(normals, axis[..., None], sign[..., None], axis=-1)
    return np.where(hit, entry, np.inf), normals


def _trace_objects(
    scene: Scene,
    camera: Camera,
    origins: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    shape = directions.shape[:2]
    best_t = np.full(shape, np.inf, dtype=np.float64)
    best_id = np.full(shape, -1, dtype=np.int16)
    best_normal = np.zeros_like(directions)
    best_albedo = np.zeros((*shape, 3), dtype=np.float64)
    best_specular = np.zeros(shape, dtype=np.float64)
    best_roughness = np.ones(shape, dtype=np.float64)

    for index, obj in enumerate(scene.objects):
        if obj["kind"] == "sphere":
            t, normal = _sphere_intersection(
                origins, directions, obj["center"], float(obj["radius"]), camera.near, camera.far
            )
        elif obj["kind"] == "plane":
            t, normal = _plane_intersection(
                origins,
                directions,
                obj["point"],
                obj["normal"],
                camera.near,
                camera.far,
            )
        else:
            t, normal = _box_intersection(
                origins,
                directions,
                obj["center"],
                obj["half_extents"],
                camera.near,
                camera.far,
            )
        hit = t < best_t
        best_t = np.where(hit, t, best_t)
        best_id = np.where(hit, index, best_id)
        best_normal = np.where(hit[..., None], normal, best_normal)
        albedo = np.asarray(obj["color"], dtype=np.float64)
        best_albedo = np.where(hit[..., None], albedo[None, None, :], best_albedo)
        best_specular = np.where(hit, float(obj.get("specular", 0.3)), best_specular)
        best_roughness = np.where(hit, float(obj.get("roughness", 0.6)), best_roughness)
    return best_t, best_id, best_normal, best_albedo, best_specular, best_roughness


def _any_shadow_hit(
    scene: Scene,
    origins: np.ndarray,
    directions: np.ndarray,
    *,
    near: float = 1e-4,
    far: float = 1000.0,
) -> np.ndarray:
    """Return a boolean mask for rays occluded by any scene object."""
    output_shape = origins.shape[:-1]
    flat_origins = origins.reshape(-1, 3)
    flat_directions = directions.reshape(-1, 3)
    occluded = np.zeros(flat_directions.shape[0], dtype=bool)
    for obj in scene.objects:
        if obj["kind"] == "sphere":
            t, _ = _sphere_intersection(
                flat_origins,
                flat_directions,
                obj["center"],
                float(obj["radius"]),
                near,
                far,
            )
        elif obj["kind"] == "plane":
            t, _ = _plane_intersection(
                flat_origins,
                flat_directions,
                obj["point"],
                obj["normal"],
                near,
                far,
            )
        else:
            t, _ = _box_intersection(
                flat_origins,
                flat_directions,
                obj["center"],
                obj["half_extents"],
                near,
                far,
            )
        occluded |= np.isfinite(t)
    return occluded.reshape(output_shape)


def shade_scene(
    scene: Scene,
    camera: Camera,
    width: int,
    height: int,
    *,
    x0: int = 0,
    y0: int = 0,
    region_width: int | None = None,
    region_height: int | None = None,
    quality: ShadingQuality = "exact",
) -> ShadeResult:
    origins, directions = make_rays(
        camera,
        width,
        height,
        x0=x0,
        y0=y0,
        region_width=region_width,
        region_height=region_height,
    )
    depth, object_id, normals, albedo, specular, roughness = _trace_objects(
        scene, camera, origins, directions
    )
    background = _background(
        scene,
        camera,
        width,
        height,
        x0,
        y0,
        region_width=region_width,
        region_height=region_height,
    )
    hit_mask = object_id >= 0
    facing = np.sum(normals * (-directions), axis=-1)
    normals = np.where((facing < 0.0)[..., None], -normals, normals)

    rgb = background.copy()
    ambient = 0.18
    light_rgb = np.stack(
        [
            np.asarray(light["color"], dtype=np.float64)
            * float(light.get("intensity", 1.0))
            for light in scene.lights
        ],
        axis=0,
    )
    safe_depth = np.where(hit_mask, depth, 0.0)
    hit_points = origins + directions * safe_depth[..., None]
    for light_index, light in enumerate(scene.lights):
        light_dir = np.asarray(light["direction"], dtype=np.float64)
        light_dir = light_dir / max(np.linalg.norm(light_dir), 1e-12)
        diffuse = np.maximum(np.sum(normals * light_dir[None, None, :], axis=-1), 0.0)
        shadow_origins = hit_points + normals * 1e-3
        shadow_dirs = np.broadcast_to(light_dir[None, None, :], origins.shape)
        shadowed = _any_shadow_hit(scene, shadow_origins, shadow_dirs)
        visibility = np.where(shadowed, 0.66, 1.0)
        half_dir = light_dir[None, None, :] - directions
        half_len = np.sqrt(np.sum(half_dir * half_dir, axis=-1, keepdims=True))
        half_dir = half_dir / np.maximum(half_len, 1e-12)
        term = albedo * (
            ambient / len(scene.lights) + diffuse[..., None] * visibility[..., None]
        )
        if quality == "exact":
            shininess = 8.0 + (1.0 - np.clip(roughness, 0.0, 1.0)) * 72.0
            spec_angle = np.maximum(np.sum(normals * half_dir, axis=-1), 0.0)
            spec = np.power(spec_angle, shininess) * specular
            rim = np.power(
                np.clip(
                    1.0 - np.maximum(np.sum(normals * (-directions), axis=-1), 0.0),
                    0.0,
                    1.0,
                ),
                3.0,
            ) * 0.16
            term += light_rgb[light_index][None, None, :] * spec[..., None]
            term += albedo * rim[..., None]
        rgb = np.where(hit_mask[..., None], term, rgb)

    fog = np.exp(-np.maximum(depth, 0.0) * 0.018)
    fog[~hit_mask] = 1.0
    rgb = rgb * fog[..., None] + background * (1.0 - fog[..., None]) * 0.42
    # A tiny stable filmic shoulder prevents clipped highlights while keeping
    # every pixel a pure function of scene, camera and pixel coordinates.
    rgb = rgb / (1.0 + np.maximum(rgb, 0.0) * 0.10)
    if quality == "preview":
        # Preview omits the exact path's specular and rim terms.  This fixed,
        # scene-independent exposure compensation keeps the low-resolution
        # level histogram aligned without introducing time/thread state.
        rgb = np.where(hit_mask[..., None], rgb * 1.07, rgb)
    rgb = _as_float32(rgb)
    return ShadeResult(
        rgb=rgb,
        depth=depth.astype(np.float32),
        object_id=object_id,
        normals=normals.astype(np.float32),
    )


def scene_has_required_geometry(scene: Scene) -> bool:
    kinds = {str(obj["kind"]) for obj in scene.objects}
    return {"sphere", "plane", "box"}.issubset(kinds)
