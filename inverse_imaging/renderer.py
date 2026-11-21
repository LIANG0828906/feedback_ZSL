"""Differentiable analytic ray casting, soft shadows and chunk gradients."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Any, Mapping

import numpy as np

from . import autodiff as ad
from .params import GROUPS, ParameterSet, canonical_json_bytes
from .scene import Scene


RAY_EPSILON = 1.0e-6
TANGENT_EPSILON = 1.0e-5


@dataclass(frozen=True)
class PixelChunk:
    index: int
    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    @property
    def pixel_count(self) -> int:
        return self.width * self.height

    @property
    def key(self) -> tuple[int, int, int, int]:
        return (self.y0, self.x0, self.y1, self.x1)


@dataclass
class ChunkGradient:
    chunk: PixelChunk
    loss: float
    gradients: dict[str, np.ndarray]
    affected_nodes: tuple[str, ...] = ()
    reused_nodes: tuple[str, ...] = ()
    recomputed_nodes: tuple[str, ...] = ()
    gradients_sha256: str = ""


@dataclass
class ForwardChunk:
    rays: tuple[np.ndarray, np.ndarray]
    intersections: tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]
    visibility: np.ndarray
    shadow_factor: np.ndarray
    shading: np.ndarray
    loss: float
    stage_hashes: dict[str, str]


def make_chunks(width: int, height: int, chunk_size: int | tuple[int, int]) -> list[PixelChunk]:
    if isinstance(chunk_size, tuple):
        chunk_w, chunk_h = chunk_size
    else:
        chunk_w = chunk_h = int(chunk_size)
    if chunk_w <= 0 or chunk_h <= 0:
        raise ValueError("chunk dimensions must be positive")
    chunks: list[PixelChunk] = []
    index = 0
    for y0 in range(0, height, chunk_h):
        for x0 in range(0, width, chunk_w):
            chunks.append(
                PixelChunk(
                    index=index,
                    x0=x0,
                    y0=y0,
                    x1=min(width, x0 + chunk_w),
                    y1=min(height, y0 + chunk_h),
                )
            )
            index += 1
    return chunks


def camera_matrix(values: Mapping[str, float]) -> np.ndarray:
    yaw = values["rotation.0"]
    pitch = values["rotation.1"]
    roll = values["rotation.2"]
    cy, sy = math.cos(yaw), math.sin(yaw)
    cp, sp = math.cos(pitch), math.sin(pitch)
    cr, sr = math.cos(roll), math.sin(roll)
    return np.asarray(
        [
            [cy * cp, cy * sp * sr - sy * cr, cy * sp * cr + sy * sr],
            [sy * cp, sy * sp * sr + cy * cr, sy * sp * cr - cy * sr],
            [-sp, cp * sr, cp * cr],
        ],
        dtype=np.float64,
    )


def generate_rays(
    params: ParameterSet, width: int, height: int, chunk: PixelChunk
) -> tuple[np.ndarray, np.ndarray]:
    camera = params.values["camera"]
    origin = np.asarray(
        [camera["position.0"], camera["position.1"], camera["position.2"]],
        dtype=np.float64,
    )
    rotation = camera_matrix(camera)
    xs = np.arange(chunk.x0, chunk.x1, dtype=np.float64) + 0.5
    ys = np.arange(chunk.y0, chunk.y1, dtype=np.float64) + 0.5
    grid_x, grid_y = np.meshgrid(xs, ys)
    local = np.stack(
        [
            (grid_x - camera["cx"]) / camera["fx"],
            -(grid_y - camera["cy"]) / camera["fy"],
            -np.ones_like(grid_x),
        ],
        axis=-1,
    )
    local /= np.linalg.norm(local, axis=-1, keepdims=True)
    directions = local @ rotation.T
    origins = np.broadcast_to(origin, directions.shape).copy()
    return origins, directions


def _geometry_arrays(
    params: ParameterSet,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    geometry = params.values["geometry"]
    count = params.sphere_count
    centers = np.asarray(
        [
            [geometry[f"sphere.{index}.center.{axis}"] for axis in range(3)]
            for index in range(count)
        ],
        dtype=np.float64,
    )
    radii = np.asarray(
        [geometry[f"sphere.{index}.radius"] for index in range(count)],
        dtype=np.float64,
    )
    plane_point = np.asarray(
        [geometry[f"plane.point.{axis}"] for axis in range(3)], dtype=np.float64
    )
    plane_normal = np.asarray(
        [geometry[f"plane.normal.{axis}"] for axis in range(3)], dtype=np.float64
    )
    plane_normal /= np.linalg.norm(plane_normal)
    return centers, radii, plane_point, plane_normal


def intersect_scene(
    origins: np.ndarray,
    directions: np.ndarray,
    params: ParameterSet,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    centers, radii, plane_point, plane_normal = _geometry_arrays(params)
    count = len(radii)
    shape = directions.shape[:-1]
    best_t = np.full(shape, np.inf, dtype=np.float64)
    best_object = np.full(shape, -1, dtype=np.int32)
    for index in range(count):
        oc = origins - centers[index]
        half_b = np.einsum("...i,...i->...", oc, directions)
        c = np.einsum("...i,...i->...", oc, oc) - radii[index] ** 2
        discriminant = half_b * half_b - c
        valid = discriminant >= 0.0
        root = np.sqrt(np.maximum(discriminant, 0.0))
        near = -half_b - root
        far = -half_b + root
        t = np.where(near > RAY_EPSILON, near, far)
        valid &= t > RAY_EPSILON
        selected = valid & (t < best_t)
        best_t = np.where(selected, t, best_t)
        best_object = np.where(selected, index, best_object)

    denominator = np.einsum("...i,i->...", directions, plane_normal)
    numerator = np.einsum("i,...i->...", plane_normal, plane_point - origins)
    with np.errstate(divide="ignore", invalid="ignore"):
        plane_t = numerator / denominator
    plane_valid = np.abs(denominator) > TANGENT_EPSILON
    plane_valid &= plane_t > RAY_EPSILON
    plane_valid &= plane_t < best_t
    best_t = np.where(plane_valid, plane_t, best_t)
    best_object = np.where(plane_valid, count, best_object)

    hit = best_object >= 0
    points = origins + np.where(hit, best_t, 0.0)[..., None] * directions
    normals = np.zeros_like(directions)
    for index in range(count):
        mask = best_object == index
        if np.any(mask):
            normals[mask] = (points[mask] - centers[index]) / radii[index]
    plane_mask = best_object == count
    if np.any(plane_mask):
        normals[plane_mask] = plane_normal
    normals = np.where(
        np.einsum("...i,...i->...", normals, directions)[..., None] > 0.0,
        -normals,
        normals,
    )
    normals[~hit] = 0.0
    return points, normals, best_object, np.where(hit, best_t, np.inf), hit


def _shadow_offsets(sample_count: int) -> list[tuple[float, float]]:
    if sample_count <= 1:
        return [(0.0, 0.0)]
    offsets = [(0.0, 0.0)]
    remaining = sample_count - 1
    ring = 1
    while remaining > 0:
        count = min(8 * ring, remaining)
        radius = 0.55 + 0.55 * (ring - 1)
        for index in range(count):
            angle = 2.0 * math.pi * index / count + 0.173 * ring
            offsets.append((radius * math.cos(angle), radius * math.sin(angle)))
        remaining -= count
        ring += 1
    return offsets[:sample_count]


def _light_basis(light_direction: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    axis = (
        np.asarray([0.0, 0.0, 1.0], dtype=np.float64)
        if abs(float(light_direction[2])) < 0.9
        else np.asarray([0.0, 1.0, 0.0], dtype=np.float64)
    )
    tangent = np.cross(light_direction, axis)
    tangent /= np.linalg.norm(tangent)
    bitangent = np.cross(light_direction, tangent)
    return tangent, bitangent


def shadow_factors(
    points: np.ndarray,
    normals: np.ndarray,
    hit: np.ndarray,
    params: ParameterSet,
    softness: float,
    sample_count: int,
) -> np.ndarray:
    centers, radii, plane_point, plane_normal = _geometry_arrays(params)
    light = params.values["light"]
    light_direction = np.asarray(
        [light[f"direction.{axis}"] for axis in range(3)], dtype=np.float64
    )
    light_direction /= np.linalg.norm(light_direction)
    tangent, bitangent = _light_basis(light_direction)
    offsets = _shadow_offsets(sample_count)
    visible = np.zeros(points.shape[:-1], dtype=np.float64)
    origins = points + normals * RAY_EPSILON * 4.0
    for u, v in offsets:
        sample_dir = light_direction + softness * (u * tangent + v * bitangent)
        sample_dir /= np.linalg.norm(sample_dir)
        occluded = np.zeros(points.shape[:-1], dtype=bool)
        for index in range(len(radii)):
            oc = origins - centers[index]
            half_b = np.einsum("...i,...i->...", oc, sample_dir)
            c = np.einsum("...i,...i->...", oc, oc) - radii[index] ** 2
            disc = half_b * half_b - c
            root = np.sqrt(np.maximum(disc, 0.0))
            near = -half_b - root
            far = -half_b + root
            t = np.where(near > RAY_EPSILON, near, far)
            occluded |= (disc >= 0.0) & (t > RAY_EPSILON)
        denominator = sample_dir @ plane_normal
        if abs(float(denominator)) > TANGENT_EPSILON:
            numerator_array = np.einsum("i,...i->...", plane_normal, plane_point - origins)
            with np.errstate(divide="ignore", invalid="ignore"):
                t = numerator_array / denominator
            occluded |= (t > RAY_EPSILON) & np.isfinite(t)
        visible += (~occluded).astype(np.float64)
    visible /= float(sample_count)
    visible[~hit] = 1.0
    return visible


def shade_image(
    points: np.ndarray,
    normals: np.ndarray,
    object_ids: np.ndarray,
    hit: np.ndarray,
    shadow: np.ndarray,
    params: ParameterSet,
) -> np.ndarray:
    light = params.values["light"]
    light_direction = np.asarray(
        [light[f"direction.{axis}"] for axis in range(3)], dtype=np.float64
    )
    light_direction /= np.linalg.norm(light_direction)
    ndotl = np.maximum(np.einsum("...i,i->...", normals, light_direction), 0.0)
    material = params.values["material"]
    image = np.zeros((*hit.shape, 3), dtype=np.float64)
    for index in range(params.sphere_count):
        mask = object_ids == index
        if not np.any(mask):
            continue
        albedo = np.asarray(
            [material[f"sphere.{index}.albedo.{channel}"] for channel in range(3)],
            dtype=np.float64,
        )
        factor = material["ambient"] + light["intensity"] * ndotl[mask] * shadow[mask]
        image[mask] = factor[:, None] * albedo[None, :]
    plane_mask = object_ids == params.sphere_count
    if np.any(plane_mask):
        albedo = np.asarray(
            [material[f"plane.albedo.{channel}"] for channel in range(3)],
            dtype=np.float64,
        )
        factor = (
            material["ambient"]
            + light["intensity"] * ndotl[plane_mask] * shadow[plane_mask]
        )
        image[plane_mask] = factor[:, None] * albedo[None, :]
    image[~hit] = 0.0
    return image


def _array_hash(*arrays: np.ndarray) -> str:
    digest = hashlib.sha256()
    for array in arrays:
        contiguous = np.ascontiguousarray(array)
        digest.update(str(contiguous.shape).encode("ascii"))
        digest.update(str(contiguous.dtype).encode("ascii"))
        digest.update(contiguous.tobytes())
    return digest.hexdigest()


def forward_chunk(
    params: ParameterSet,
    scene: Scene,
    chunk: PixelChunk,
    target: np.ndarray | None = None,
) -> ForwardChunk:
    rays = generate_rays(params, scene.width, scene.height, chunk)
    intersections = intersect_scene(*rays, params)
    points, normals, object_ids, _, hit = intersections
    shadow = shadow_factors(
        points,
        normals,
        hit,
        params,
        scene.settings.softness,
        scene.settings.shadow_samples,
    )
    image = shade_image(points, normals, object_ids, hit, shadow, params)
    target_chunk = (
        scene.target[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
        if target is None
        else target[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
    )
    difference = image - target_chunk
    loss = float(np.sum(difference * difference) / (scene.width * scene.height * 3))
    stage_hashes = {
        "camera_rays": _array_hash(*rays),
        "intersections": _array_hash(points, normals, object_ids.astype(np.float64), hit.astype(np.float64)),
        "visibility": _array_hash(hit.astype(np.float64), object_ids.astype(np.float64)),
        "shadow_factor": _array_hash(shadow),
        "shading": _array_hash(image),
    }
    stage_hashes["loss"] = _array_hash(np.asarray([loss], dtype=np.float64))
    return ForwardChunk(
        rays=rays,
        intersections=intersections,
        visibility=hit,
        shadow_factor=shadow,
        shading=image,
        loss=loss,
        stage_hashes=stage_hashes,
    )


def render_image(params: ParameterSet, scene: Scene, chunk_size: int = 32) -> np.ndarray:
    image = np.zeros_like(scene.target)
    for chunk in make_chunks(scene.width, scene.height, chunk_size):
        forward = forward_chunk(params, scene, chunk)
        image[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1] = forward.shading
    return image


def forward_scene(
    params: ParameterSet,
    scene: Scene,
    chunk_size: int = 32,
) -> dict[str, object]:
    """Return the scalar loss plus all differentiable forward intermediates."""
    rays_origin = np.zeros((scene.height, scene.width, 3), dtype=np.float64)
    rays_direction = np.zeros_like(rays_origin)
    points = np.zeros_like(rays_origin)
    normals = np.zeros_like(rays_origin)
    object_ids = np.full((scene.height, scene.width), -1, dtype=np.int32)
    depths = np.full((scene.height, scene.width), np.inf, dtype=np.float64)
    visibility = np.zeros((scene.height, scene.width), dtype=bool)
    shadow = np.zeros((scene.height, scene.width), dtype=np.float64)
    shading = np.zeros_like(rays_origin)
    loss = 0.0
    for chunk in make_chunks(scene.width, scene.height, chunk_size):
        forward = forward_chunk(params, scene, chunk)
        y = slice(chunk.y0, chunk.y1)
        x = slice(chunk.x0, chunk.x1)
        rays_origin[y, x] = forward.rays[0]
        rays_direction[y, x] = forward.rays[1]
        chunk_points, chunk_normals, chunk_objects, chunk_depths, chunk_hit = (
            forward.intersections
        )
        points[y, x] = chunk_points
        normals[y, x] = chunk_normals
        object_ids[y, x] = chunk_objects
        depths[y, x] = chunk_depths
        visibility[y, x] = chunk_hit
        shadow[y, x] = forward.shadow_factor
        shading[y, x] = forward.shading
        loss += forward.loss
    return {
        "loss": loss,
        "shading": shading,
        "intersections": {
            "points": points,
            "normals": normals,
            "object_ids": object_ids,
            "depths": depths,
        },
        "visibility": visibility,
        "shadow_factor": shadow,
        "lighting": shading,
        "rays": {"origins": rays_origin, "directions": rays_direction},
    }


def _ad_root_map(params: ParameterSet) -> dict[str, dict[str, ad.Var]]:
    return {
        group: {key: ad.Var.root(params.values[group][key]) for key in params.keys(group)}
        for group in GROUPS
    }


def _ad_camera_ray(
    roots: Mapping[str, Mapping[str, ad.Var]], px: float, py: float
) -> tuple[tuple[ad.Var, ad.Var, ad.Var], tuple[ad.Var, ad.Var, ad.Var]]:
    camera = roots["camera"]
    origin = (camera["position.0"], camera["position.1"], camera["position.2"])
    yaw, pitch, roll = camera["rotation.0"], camera["rotation.1"], camera["rotation.2"]
    cy, sy = ad.cos(yaw), ad.sin(yaw)
    cp, sp = ad.cos(pitch), ad.sin(pitch)
    cr, sr = ad.cos(roll), ad.sin(roll)
    row0 = (cy * cp, cy * sp * sr - sy * cr, cy * sp * cr + sy * sr)
    row1 = (sy * cp, sy * sp * sr + cy * cr, sy * sp * cr - cy * sr)
    row2 = (-sp, cp * sr, cp * cr)
    x_camera = (px + 0.5 - camera["cx"]) / camera["fx"]
    y_camera = -(py + 0.5 - camera["cy"]) / camera["fy"]
    local = (x_camera, y_camera, ad.Var.constant(-1.0))
    inverse_norm = ad.Var.constant(1.0) / ad.norm3(local)
    local = (local[0] * inverse_norm, local[1] * inverse_norm, local[2] * inverse_norm)

    def apply_row(row: tuple[ad.Var, ad.Var, ad.Var]) -> ad.Var:
        return row[0] * local[0] + row[1] * local[1] + row[2] * local[2]

    return origin, (apply_row(row0), apply_row(row1), apply_row(row2))


def _ad_intersect(
    origin: tuple[ad.Var, ad.Var, ad.Var],
    direction: tuple[ad.Var, ad.Var, ad.Var],
    roots: Mapping[str, Mapping[str, ad.Var]],
    sphere_count: int,
    include_plane: bool = True,
) -> tuple[float, int, tuple[ad.Var, ad.Var, ad.Var] | None]:
    geometry = roots["geometry"]
    best_t = math.inf
    best_object = -1
    best_t_var: ad.Var | None = None
    for index in range(sphere_count):
        center = (
            geometry[f"sphere.{index}.center.0"],
            geometry[f"sphere.{index}.center.1"],
            geometry[f"sphere.{index}.center.2"],
        )
        radius = geometry[f"sphere.{index}.radius"]
        oc = (
            origin[0] - center[0],
            origin[1] - center[1],
            origin[2] - center[2],
        )
        half_b = ad.dot3(oc, direction)
        c = ad.dot3(oc, oc) - radius * radius
        discriminant = half_b * half_b - c
        if discriminant.value < 0.0:
            continue
        root = ad.sqrt(discriminant)
        near = -half_b - root
        far = -half_b + root
        t_var = near if near.value > RAY_EPSILON else far
        if t_var.value > RAY_EPSILON and t_var.value < best_t:
            best_t = t_var.value
            best_object = index
            best_t_var = t_var
    if include_plane:
        point = (
            geometry["plane.point.0"],
            geometry["plane.point.1"],
            geometry["plane.point.2"],
        )
        normal = (
            geometry["plane.normal.0"],
            geometry["plane.normal.1"],
            geometry["plane.normal.2"],
        )
        normal_length = ad.norm3(normal)
        unit_normal = (
            normal[0] / normal_length,
            normal[1] / normal_length,
            normal[2] / normal_length,
        )
        denominator = ad.dot3(direction, unit_normal)
        numerator = ad.dot3(
            unit_normal,
            (point[0] - origin[0], point[1] - origin[1], point[2] - origin[2]),
        )
        if abs(denominator.value) > TANGENT_EPSILON:
            t_var = numerator / denominator
            if t_var.value > RAY_EPSILON and t_var.value < best_t:
                best_t = t_var.value
                best_object = sphere_count
                best_t_var = t_var
    if best_object < 0:
        return math.inf, -1, None
    assert best_t_var is not None
    return (
        best_t,
        best_object,
        (
            origin[0] + best_t_var * direction[0],
            origin[1] + best_t_var * direction[1],
            origin[2] + best_t_var * direction[2],
        ),
    )


def _ad_unit_normal(
    point: tuple[ad.Var, ad.Var, ad.Var],
    object_id: int,
    roots: Mapping[str, Mapping[str, ad.Var]],
    sphere_count: int,
) -> tuple[ad.Var, ad.Var, ad.Var]:
    geometry = roots["geometry"]
    if object_id < sphere_count:
        center = (
            geometry[f"sphere.{object_id}.center.0"],
            geometry[f"sphere.{object_id}.center.1"],
            geometry[f"sphere.{object_id}.center.2"],
        )
        radius = geometry[f"sphere.{object_id}.radius"]
        normal = (
            (point[0] - center[0]) / radius,
            (point[1] - center[1]) / radius,
            (point[2] - center[2]) / radius,
        )
    else:
        raw = (
            geometry["plane.normal.0"],
            geometry["plane.normal.1"],
            geometry["plane.normal.2"],
        )
        length = ad.norm3(raw)
        normal = (raw[0] / length, raw[1] / length, raw[2] / length)
    return normal


def _ad_shadow_visible(
    point: tuple[ad.Var, ad.Var, ad.Var],
    normal: tuple[ad.Var, ad.Var, ad.Var],
    direction: tuple[ad.Var, ad.Var, ad.Var],
    roots: Mapping[str, Mapping[str, ad.Var]],
    sphere_count: int,
    sample_u: float,
    sample_v: float,
    softness: float,
) -> int:
    light = roots["light"]
    raw_light = (light["direction.0"], light["direction.1"], light["direction.2"])
    light_norm = ad.norm3(raw_light)
    light_direction = (
        raw_light[0] / light_norm,
        raw_light[1] / light_norm,
        raw_light[2] / light_norm,
    )
    axis = (
        ad.Var.constant(0.0),
        ad.Var.constant(0.0),
        ad.Var.constant(1.0),
    )
    if abs(light_direction[2].value) >= 0.9:
        axis = (
            ad.Var.constant(0.0),
            ad.Var.constant(1.0),
            ad.Var.constant(0.0),
        )
    tangent = (
        light_direction[1] * axis[2] - light_direction[2] * axis[1],
        light_direction[2] * axis[0] - light_direction[0] * axis[2],
        light_direction[0] * axis[1] - light_direction[1] * axis[0],
    )
    tangent_norm = ad.norm3(tangent)
    tangent = (
        tangent[0] / tangent_norm,
        tangent[1] / tangent_norm,
        tangent[2] / tangent_norm,
    )
    bitangent = (
        light_direction[1] * tangent[2] - light_direction[2] * tangent[1],
        light_direction[2] * tangent[0] - light_direction[0] * tangent[2],
        light_direction[0] * tangent[1] - light_direction[1] * tangent[0],
    )
    sample_direction = (
        light_direction[0] + softness * (sample_u * tangent[0] + sample_v * bitangent[0]),
        light_direction[1] + softness * (sample_u * tangent[1] + sample_v * bitangent[1]),
        light_direction[2] + softness * (sample_u * tangent[2] + sample_v * bitangent[2]),
    )
    sample_norm = ad.norm3(sample_direction)
    sample_direction = (
        sample_direction[0] / sample_norm,
        sample_direction[1] / sample_norm,
        sample_direction[2] / sample_norm,
    )
    origin = (
        point[0] + normal[0] * (RAY_EPSILON * 4.0),
        point[1] + normal[1] * (RAY_EPSILON * 4.0),
        point[2] + normal[2] * (RAY_EPSILON * 4.0),
    )
    _, object_id, _ = _ad_intersect(
        origin, sample_direction, roots, sphere_count, include_plane=True
    )
    return 0 if object_id >= 0 else 1


def _pixel_gradients(
    roots: Mapping[str, Mapping[str, ad.Var]],
    scene: Scene,
    px: int,
    py: int,
) -> tuple[float, bool]:
    origin, direction = _ad_camera_ray(roots, float(px), float(py))
    _, object_id, point = _ad_intersect(
        origin, direction, roots, scene.params.sphere_count, include_plane=True
    )
    target = scene.target[py, px]
    if object_id < 0 or point is None:
        raw_loss = ad.Var.constant(0.0)
        for channel in range(3):
            raw_loss = raw_loss + (ad.Var.constant(0.0) - float(target[channel])) ** 2
        scaled_loss = raw_loss * (1.0 / (scene.width * scene.height * 3.0))
        scaled_loss.backward()
        return float(scaled_loss.value), False

    normal = _ad_unit_normal(point, object_id, roots, scene.params.sphere_count)
    if ad.dot3(normal, direction).value > 0.0:
        normal = (-normal[0], -normal[1], -normal[2])

    light = roots["light"]
    raw_light = (light["direction.0"], light["direction.1"], light["direction.2"])
    light_norm = ad.norm3(raw_light)
    light_direction = (
        raw_light[0] / light_norm,
        raw_light[1] / light_norm,
        raw_light[2] / light_norm,
    )
    offsets = _shadow_offsets(scene.settings.shadow_samples)
    visibility_count = 0
    for sample_u, sample_v in offsets:
        visibility_count += _ad_shadow_visible(
            point,
            normal,
            direction,
            roots,
            scene.params.sphere_count,
            sample_u,
            sample_v,
            scene.settings.softness,
        )
    shadow = ad.Var.constant(visibility_count / float(len(offsets)))
    ndotl = ad.maximum(ad.dot3(normal, light_direction), 0.0)
    material = roots["material"]
    intensity = light["intensity"]
    ambient = material["ambient"]
    factor = ambient + intensity * ndotl * shadow
    raw_loss = ad.Var.constant(0.0)
    for channel in range(3):
        if object_id < scene.params.sphere_count:
            albedo = material[f"sphere.{object_id}.albedo.{channel}"]
        else:
            albedo = material[f"plane.albedo.{channel}"]
        value = albedo * factor
        raw_loss = raw_loss + (value - float(target[channel])) ** 2
    scaled_loss = raw_loss * (1.0 / (scene.width * scene.height * 3.0))
    scaled_loss.backward()
    return float(scaled_loss.value), True


def gradient_chunk(
    params: ParameterSet,
    scene: Scene,
    chunk: PixelChunk,
    affected_nodes: tuple[str, ...] = (),
    reused_nodes: tuple[str, ...] = (),
    recomputed_nodes: tuple[str, ...] = (),
) -> ChunkGradient:
    roots = _ad_root_map(params)
    loss = 0.0
    for py in range(chunk.y0, chunk.y1):
        for px in range(chunk.x0, chunk.x1):
            pixel_loss, _ = _pixel_gradients(roots, scene, px, py)
            loss += pixel_loss
    gradients: dict[str, np.ndarray] = {}
    for group in GROUPS:
        gradients[group] = np.asarray(
            [roots[group][key].grad for key in params.keys(group)], dtype=np.float64
        )
    return ChunkGradient(
        chunk=chunk,
        loss=loss,
        gradients=gradients,
        affected_nodes=affected_nodes,
        reused_nodes=reused_nodes,
        recomputed_nodes=recomputed_nodes,
        gradients_sha256=chunk_gradient_hash(gradients),
    )


def chunk_gradient_hash(gradients: Mapping[str, Any]) -> str:
    digest = hashlib.sha256()
    for group in GROUPS:
        digest.update(group.encode("ascii"))
        digest.update(np.ascontiguousarray(gradients[group]).tobytes())
    return digest.hexdigest()


def gradient_content_hash(gradients: Mapping[str, Any]) -> str:
    digest = hashlib.sha256()
    for group in GROUPS:
        if isinstance(gradients[group], np.ndarray):
            values = gradients[group].tolist()
        else:
            values = list(gradients[group])
        digest.update(group.encode("ascii"))
        digest.update(canonical_json_bytes(values))
    return digest.hexdigest()

