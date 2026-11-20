"""Analytic differentiable sphere/plane rendering used by the fitter."""

from __future__ import annotations

from collections import OrderedDict
from typing import Mapping

import numpy as np
import torch

from .params import ParameterStore
from .scene import Scene


TensorMap = OrderedDict[str, torch.Tensor]

_EPS = 1e-9
_SHADOW_BIAS = 2e-5


def _parameter(store: ParameterStore, path: str) -> torch.Tensor:
    return store.parameters[path]


def _normalize(vector: torch.Tensor, *, dim: int = -1) -> torch.Tensor:
    norm = torch.linalg.vector_norm(vector, dim=dim, keepdim=True)
    return vector / torch.clamp(norm, min=1e-12)


def _stack_vectors(
    store: ParameterStore,
    paths: list[str],
) -> torch.Tensor:
    return torch.stack([_parameter(store, path) for path in paths], dim=0)


def _stack_scalars(
    store: ParameterStore,
    paths: list[str],
) -> torch.Tensor:
    return torch.stack([_parameter(store, path) for path in paths], dim=0)


def _select_by_object(
    object_ids: torch.Tensor,
    stacked: torch.Tensor,
) -> torch.Tensor:
    """Select (H, W, N, ...) values without differentiating through argmin."""

    selected = torch.zeros_like(stacked[:, :, 0])
    for index in range(stacked.shape[2]):
        mask = (object_ids == index).unsqueeze(-1)
        selected = torch.where(mask, stacked[:, :, index], selected)
    return selected


def camera_rays(
    store: ParameterStore,
    scene: Scene,
) -> tuple[torch.Tensor, torch.Tensor]:
    position = _parameter(store, "camera.position")
    target = _parameter(store, "camera.target")
    up = _parameter(store, "camera.up")
    fov_deg = _parameter(store, "camera.fov_deg")

    forward = _normalize(target - position)
    right = _normalize(torch.linalg.cross(forward, up, dim=-1))
    true_up = torch.linalg.cross(right, forward, dim=-1)

    width = scene.width
    height = scene.height
    x = (torch.arange(width, dtype=torch.float64) + 0.5) / width * 2.0 - 1.0
    y = 1.0 - (torch.arange(height, dtype=torch.float64) + 0.5) / height * 2.0
    yy, xx = torch.meshgrid(y, x, indexing="ij")
    half_height = torch.tan(torch.deg2rad(fov_deg) * 0.5)
    aspect = float(width) / float(height)
    directions = (
        forward[None, None, :]
        + xx[..., None] * aspect * half_height * right[None, None, :]
        + yy[..., None] * half_height * true_up[None, None, :]
    )
    directions = _normalize(directions)
    origins = position.expand_as(directions)
    return origins, directions


def compute_intersections(
    store: ParameterStore,
    scene: Scene,
) -> TensorMap:
    origins, directions = camera_rays(store, scene)
    height, width = scene.height, scene.width

    sphere_centers = _stack_vectors(
        store, [f"geometry.{sphere.name}.center" for sphere in scene.spheres]
    )
    sphere_radii = _stack_scalars(
        store, [f"geometry.{sphere.name}.radius" for sphere in scene.spheres]
    )

    relative = origins[:, :, None, :] - sphere_centers[None, None, :, :]
    half_b = torch.sum(relative * directions[:, :, None, :], dim=-1)
    c_term = torch.sum(relative * relative, dim=-1) - sphere_radii[None, None, :] ** 2
    discriminant = half_b * half_b - c_term
    root = torch.sqrt(torch.clamp(discriminant, min=0.0))
    near = -half_b - root
    far = -half_b + root
    sphere_t = torch.where(near > _SHADOW_BIAS, near, far)
    sphere_valid = (discriminant > 0.0) & (sphere_t > _SHADOW_BIAS)
    sphere_t = torch.where(sphere_valid, sphere_t, torch.full_like(sphere_t, torch.inf))

    plane_point = _parameter(store, f"geometry.{scene.plane.name}.point")
    plane_normal = _normalize(
        _parameter(store, f"geometry.{scene.plane.name}.normal"), dim=0
    )
    denominator = torch.sum(directions * plane_normal[None, None, :], dim=-1)
    plane_numerator = torch.sum(
        (plane_point[None, None, :] - origins) * plane_normal[None, None, :], dim=-1
    )
    safe_denominator = torch.where(
        torch.abs(denominator) > _EPS,
        denominator,
        torch.full_like(denominator, _EPS),
    )
    plane_t = plane_numerator / safe_denominator
    plane_valid = (torch.abs(denominator) > _EPS) & (plane_t > _SHADOW_BIAS)
    plane_t = torch.where(plane_valid, plane_t, torch.full_like(plane_t, torch.inf))

    all_t = torch.cat((sphere_t, plane_t[:, :, None]), dim=-1)
    object_ids = torch.argmin(all_t.detach(), dim=-1)
    t_hit = torch.min(all_t, dim=-1).values
    hit_mask = torch.isfinite(t_hit)
    t_safe = torch.where(hit_mask, t_hit, torch.zeros_like(t_hit))
    points = origins + t_safe[..., None] * directions

    sphere_normals: list[torch.Tensor] = []
    for index, sphere in enumerate(scene.spheres):
        center = _parameter(store, f"geometry.{sphere.name}.center")
        radius = _parameter(store, f"geometry.{sphere.name}.radius")
        normal = (points - center[None, None, :]) / torch.clamp(radius, min=1e-9)
        sphere_normals.append(_normalize(normal))
    sphere_normal_stack = torch.stack(sphere_normals, dim=2)
    if sphere_normal_stack.shape[2] + 1 != all_t.shape[-1]:
        raise RuntimeError("internal object ordering mismatch")

    sphere_normal = _select_by_object(object_ids, sphere_normal_stack)
    plane_normal_image = plane_normal[None, None, :].expand_as(sphere_normal)
    is_plane = (object_ids == len(scene.spheres)).unsqueeze(-1)
    normals = torch.where(is_plane, plane_normal_image, sphere_normal)
    normals = torch.where(hit_mask[..., None], normals, torch.zeros_like(normals))

    return OrderedDict(
        (
            ("t", t_hit),
            ("hit_mask", hit_mask),
            ("points", points),
            ("normals", normals),
            ("object_ids", object_ids),
        )
    )


def compute_visibility(
    scene: Scene,
    intersections: Mapping[str, torch.Tensor],
) -> TensorMap:
    t_hit = intersections["t"]
    hit_mask = intersections["hit_mask"]
    softness = scene.shadow_softness
    smooth_hit = torch.sigmoid((t_hit - _SHADOW_BIAS) / softness)
    visibility = torch.where(hit_mask, smooth_hit, torch.zeros_like(smooth_hit))
    return OrderedDict((("visibility", visibility),))


def compute_shadow_factor(
    store: ParameterStore,
    scene: Scene,
    intersections: Mapping[str, torch.Tensor],
    visibility: Mapping[str, torch.Tensor],
) -> TensorMap:
    points = intersections["points"]
    normals = intersections["normals"]
    light_direction = _normalize(_parameter(store, "light.direction"), dim=0)
    to_light = -light_direction
    softness = scene.shadow_softness

    # Offset toward the light-facing side of the surface to avoid self hits.
    facing = torch.sum(normals * to_light[None, None, :], dim=-1, keepdim=True)
    bias_direction = torch.where(facing >= 0.0, normals, -normals)
    shadow_origins = points + _SHADOW_BIAS * bias_direction

    sphere_centers = _stack_vectors(
        store, [f"geometry.{sphere.name}.center" for sphere in scene.spheres]
    )
    sphere_radii = _stack_scalars(
        store, [f"geometry.{sphere.name}.radius" for sphere in scene.spheres]
    )
    relative = shadow_origins[:, :, None, :] - sphere_centers[None, None, :, :]
    half_b = torch.sum(relative * to_light[None, None, None, :], dim=-1)
    c_term = torch.sum(relative * relative, dim=-1) - sphere_radii[None, None, :] ** 2
    discriminant = half_b * half_b - c_term
    root = torch.sqrt(torch.clamp(discriminant, min=0.0))
    near = -half_b - root
    disc_scale = softness * (2.0 * torch.clamp(sphere_radii, min=softness) + softness)
    chord_score = torch.sigmoid(discriminant / disc_scale[None, None, :])
    near_score = torch.sigmoid((near - _SHADOW_BIAS) / softness)
    sphere_occlusion = torch.clamp(chord_score * near_score, 0.0, 1.0)
    transmittance = torch.prod(1.0 - sphere_occlusion, dim=-1)

    plane_point = _parameter(store, f"geometry.{scene.plane.name}.point")
    plane_normal = _normalize(
        _parameter(store, f"geometry.{scene.plane.name}.normal"), dim=0
    )
    denominator = torch.sum(to_light * plane_normal, dim=-1)
    numerator = torch.sum(
        (plane_point[None, None, :] - shadow_origins) * plane_normal[None, None, :],
        dim=-1,
    )
    safe_denominator = torch.where(
        torch.abs(denominator) > _EPS,
        denominator,
        torch.full_like(denominator, _EPS),
    )
    plane_t = numerator / safe_denominator
    plane_hit_score = torch.sigmoid(
        torch.where(
            torch.abs(denominator) > _EPS,
            (plane_t - _SHADOW_BIAS) / softness,
            torch.full_like(plane_t, -20.0),
        )
    )
    transmittance = transmittance * (1.0 - plane_hit_score)

    visible = visibility["visibility"]
    shadow_factor = torch.clamp(transmittance, 0.0, 1.0) * visible
    return OrderedDict((("shadow_factor", shadow_factor),))


def _material_albedos(
    store: ParameterStore,
    scene: Scene,
    object_ids: torch.Tensor,
) -> torch.Tensor:
    paths = [f"material.{name}.albedo" for name in scene.object_names()]
    stacked = torch.stack([_parameter(store, path) for path in paths], dim=0)
    stacked = stacked[None, None, :, :].expand(
        scene.height, scene.width, stacked.shape[0], 3
    )
    return _select_by_object(object_ids, stacked)


def compute_lighting(
    store: ParameterStore,
    scene: Scene,
    intersections: Mapping[str, torch.Tensor],
    visibility: Mapping[str, torch.Tensor],
    shadow: Mapping[str, torch.Tensor],
) -> TensorMap:
    normals = intersections["normals"]
    object_ids = intersections["object_ids"]
    albedo = _material_albedos(store, scene, object_ids)
    light_direction = _normalize(_parameter(store, "light.direction"), dim=0)
    to_light = -light_direction
    intensity = _parameter(store, "light.intensity")
    ndotl = torch.clamp(
        torch.sum(normals * to_light[None, None, :], dim=-1), min=0.0
    )
    rgb = (
        albedo
        * intensity
        * ndotl[..., None]
        * visibility["visibility"][..., None]
        * shadow["shadow_factor"][..., None]
        / torch.pi
    )
    return OrderedDict((("rgb", rgb),))


def compute_loss(
    scene: Scene,
    lighting: Mapping[str, torch.Tensor],
) -> TensorMap:
    target = torch.as_tensor(
        np.array(scene.target, dtype=np.float64, copy=True), dtype=torch.float64
    )
    residual = lighting["rgb"] - target
    loss = torch.mean(residual * residual)
    return OrderedDict((("loss", loss),))


def full_render(
    store: ParameterStore,
    scene: Scene,
) -> tuple[OrderedDict[str, TensorMap], torch.Tensor]:
    nodes: OrderedDict[str, TensorMap] = OrderedDict()
    nodes["intersections"] = compute_intersections(store, scene)
    nodes["visibility"] = compute_visibility(scene, nodes["intersections"])
    nodes["shadow_factor"] = compute_shadow_factor(
        store, scene, nodes["intersections"], nodes["visibility"]
    )
    nodes["lighting"] = compute_lighting(
        store,
        scene,
        nodes["intersections"],
        nodes["visibility"],
        nodes["shadow_factor"],
    )
    nodes["loss"] = compute_loss(scene, nodes["lighting"])
    return nodes, nodes["loss"]["loss"]

