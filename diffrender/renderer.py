"""Differentiable ray casting with spheres, planes, area lights and soft shadows."""

from __future__ import annotations

import math
from dataclasses import dataclass

import torch
import torch.nn.functional as F

from .errors import fail
from .params import RenderScene
from .schema import CameraSpec, TargetImageSpec


@dataclass(frozen=True)
class RenderConfig:
    shadow_samples: int = 4
    ray_epsilon: float | None = None
    downsample_factor: int = 1
    base_seed: int = 0

    def validate(self) -> None:
        if isinstance(self.shadow_samples, bool) or not isinstance(self.shadow_samples, int):
            fail(
                "E_PARA",
                "shadow_samples must be an integer",
                "shadow_samples",
                value=repr(self.shadow_samples),
            )
        if not (1 <= self.shadow_samples <= 64):
            fail(
                "E_PARA",
                "shadow_samples must be in [1, 64]",
                "shadow_samples",
                value=self.shadow_samples,
            )
        if isinstance(self.downsample_factor, bool) or not isinstance(
            self.downsample_factor, int
        ):
            fail(
                "E_PARA",
                "downsample_factor must be an integer",
                "downsample_factor",
                value=repr(self.downsample_factor),
            )
        if self.downsample_factor not in {1, 2, 4, 8, 16}:
            fail(
                "E_PARA",
                "downsample_factor must be one of 1, 2, 4, 8, 16",
                "downsample_factor",
                value=self.downsample_factor,
            )
        if self.ray_epsilon is not None:
            value = float(self.ray_epsilon)
            if not math.isfinite(value) or not (0.0 < value <= 0.1):
                fail(
                    "E_PARA",
                    "ray_epsilon must be in (0, 0.1]",
                    "ray_epsilon",
                    value=value,
                )
        if isinstance(self.base_seed, bool) or not isinstance(self.base_seed, int):
            fail("E_PARA", "base_seed must be an integer", "base_seed")


def _camera_tensors(
    camera: CameraSpec,
    dtype: torch.dtype,
    device: torch.device,
) -> tuple[torch.Tensor, torch.Tensor, torch.Tensor]:
    intrinsics = torch.tensor(camera.intrinsics, dtype=dtype, device=device)
    extrinsics = torch.tensor(camera.world_to_camera, dtype=dtype, device=device)
    rotation = extrinsics[:3, :3]
    translation = extrinsics[:3, 3]
    center = -(rotation.transpose(0, 1) @ translation)
    return intrinsics, rotation, center


def camera_rays(
    camera: CameraSpec,
    height: int,
    width: int,
    dtype: torch.dtype,
    device: torch.device,
    *,
    x_offset: int = 0,
    y_offset: int = 0,
    tile_width: int | None = None,
    tile_height: int | None = None,
    x_pixel_scale: float = 1.0,
    y_pixel_scale: float = 1.0,
) -> tuple[torch.Tensor, torch.Tensor]:
    tile_width = width if tile_width is None else tile_width
    tile_height = height if tile_height is None else tile_height
    if not (0 <= x_offset < width and 0 <= y_offset < height):
        fail("E_PARA", "tile origin is outside the image", "tile_origin")
    if not (1 <= tile_width <= width - x_offset):
        fail("E_PARA", "tile width is out of range", "tile_width")
    if not (1 <= tile_height <= height - y_offset):
        fail("E_PARA", "tile height is out of range", "tile_height")
    ys = torch.arange(
        y_offset, y_offset + tile_height, dtype=dtype, device=device
    ).view(tile_height, 1)
    xs = torch.arange(
        x_offset, x_offset + tile_width, dtype=dtype, device=device
    ).view(1, tile_width)
    ys = (ys.expand(tile_height, tile_width) + 0.5) * y_pixel_scale
    xs = (xs.expand(tile_height, tile_width) + 0.5) * x_pixel_scale
    ones = torch.ones_like(xs)
    pixels = torch.stack((xs, ys, ones), dim=-1)
    intrinsics, rotation, center = _camera_tensors(camera, dtype, device)
    try:
        inverse = torch.linalg.inv(intrinsics)
    except RuntimeError:
        fail("E_PARA", "camera intrinsics are singular", "intrinsics")
    camera_dirs = pixels @ inverse.transpose(0, 1)
    camera_dirs = F.normalize(camera_dirs, dim=-1)
    world_dirs = camera_dirs @ rotation
    origins = center.view(1, 1, 3).expand(tile_height, tile_width, 3)
    return origins, world_dirs


def _radical_inverse(index: int, base: int) -> float:
    value = 0.0
    factor = 1.0 / base
    while index > 0:
        value += factor * (index % base)
        index //= base
        factor /= base
    return value


def _sample_rect_light(
    scene: RenderScene,
    light_index: int,
    sample_count: int,
    seed: int,
) -> torch.Tensor:
    dtype = scene.light_centers.dtype
    device = scene.light_centers.device
    normal = scene.light_normals[light_index]
    reference = torch.tensor((0.0, 1.0, 0.0), dtype=dtype, device=device)
    dot = torch.abs(torch.dot(normal, reference))
    if float(dot) > 0.99:
        reference = torch.tensor((1.0, 0.0, 0.0), dtype=dtype, device=device)
    tangent_u = F.normalize(torch.cross(normal, reference, dim=0), dim=0)
    tangent_v = F.normalize(torch.cross(normal, tangent_u, dim=0), dim=0)
    shift_u = ((seed * 1664525 + 1013904223) & 0xFFFFFFFF) / 4294967296.0
    shift_v = ((seed * 22695477 + 1) & 0xFFFFFFFF) / 4294967296.0
    points: list[torch.Tensor] = []
    half_u, half_v = scene.light_half_sizes[light_index]
    for sample_index in range(sample_count):
        u = (_radical_inverse(sample_index + 1, 2) + shift_u) % 1.0
        v = (_radical_inverse(sample_index + 1, 3) + shift_v) % 1.0
        point = (
            scene.light_centers[light_index]
            + (2.0 * u - 1.0) * half_u * tangent_u
            + (2.0 * v - 1.0) * half_v * tangent_v
        )
        points.append(point)
    return torch.stack(points)


def _intersect_spheres(
    origins: torch.Tensor,
    directions: torch.Tensor,
    centers: torch.Tensor,
    radii: torch.Tensor,
    epsilon: float,
) -> tuple[torch.Tensor, torch.Tensor]:
    oc = origins.unsqueeze(-2) - centers
    b = (oc * directions.unsqueeze(-2)).sum(dim=-1)
    c = (oc * oc).sum(dim=-1) - radii.square()
    discriminant = b.square() - c
    valid_disc = discriminant > 0.0
    root = torch.sqrt(torch.clamp(discriminant, min=0.0))
    near = -b - root
    far = -b + root
    t = torch.where(near > epsilon, near, far)
    valid = valid_disc & (t > epsilon)
    return t, valid


def _intersect_planes(
    origins: torch.Tensor,
    directions: torch.Tensor,
    points: torch.Tensor,
    normals: torch.Tensor,
    epsilon: float,
) -> tuple[torch.Tensor, torch.Tensor]:
    denominator = directions @ normals.transpose(0, 1)
    delta = points.unsqueeze(0).unsqueeze(0) - origins.unsqueeze(-2)
    numerator = (delta * normals.unsqueeze(0).unsqueeze(0)).sum(dim=-1)
    safe_denominator = torch.where(
        denominator.abs() > epsilon,
        denominator,
        torch.full_like(denominator, epsilon),
    )
    t = numerator / safe_denominator
    valid = (denominator.abs() > epsilon) & (t > epsilon)
    return t, valid


def _nearest_hit(
    scene: RenderScene,
    origins: torch.Tensor,
    directions: torch.Tensor,
) -> tuple[torch.Tensor, torch.Tensor, torch.Tensor, torch.Tensor]:
    epsilon = scene.ray_epsilon
    sphere_t, sphere_valid = _intersect_spheres(
        origins,
        directions,
        scene.sphere_centers,
        scene.sphere_radii,
        epsilon,
    )
    plane_t, plane_valid = _intersect_planes(
        origins,
        directions,
        scene.plane_points,
        scene.plane_normals,
        epsilon,
    )
    t_all = torch.cat((sphere_t, plane_t), dim=-1)
    valid_all = torch.cat((sphere_valid, plane_valid), dim=-1)
    t_filled = torch.where(valid_all, t_all, torch.full_like(t_all, float("inf")))
    best_t, index = t_filled.min(dim=-1)
    active = torch.isfinite(best_t)
    safe_t = torch.where(active, best_t, torch.zeros_like(best_t))
    hit = origins + directions * safe_t.unsqueeze(-1)

    sphere_normals = (
        hit.unsqueeze(-2) - scene.sphere_centers
    ) / scene.sphere_radii.view(1, 1, -1, 1)
    sphere_normals = F.normalize(sphere_normals, dim=-1)
    plane_normals = scene.plane_normals.view(1, 1, -1, 3).expand(
        hit.shape[0], hit.shape[1], -1, -1
    )
    normals_all = torch.cat((sphere_normals, plane_normals), dim=-2)
    albedos_all = torch.cat(
        (scene.sphere_albedos, scene.plane_albedos), dim=0
    ).view(1, 1, -1, 3).expand(hit.shape[0], hit.shape[1], -1, -1)
    gather_index = index.unsqueeze(-1).unsqueeze(-1).expand(-1, -1, 1, 3)
    normals = torch.gather(normals_all, 2, gather_index).squeeze(2)
    albedos = torch.gather(albedos_all, 2, gather_index).squeeze(2)
    return hit, normals, albedos, active


def _direct_lighting(
    scene: RenderScene,
    hit: torch.Tensor,
    normals: torch.Tensor,
    albedo: torch.Tensor,
    seed: int,
    shadow_samples: int,
) -> torch.Tensor:
    if scene.light_intensities.numel() == 0:
        return albedo * scene.ambient
    light_samples = torch.stack(
        [
            _sample_rect_light(scene, light_index, shadow_samples, seed + light_index)
            for light_index in range(scene.light_centers.shape[0])
        ]
    )
    to_light = light_samples.view(1, 1, *light_samples.shape) - hit.view(
        *hit.shape[:2], 1, 1, 3
    )
    distance_sq = to_light.square().sum(dim=-1)
    distance = torch.sqrt(torch.clamp(distance_sq, min=1.0e-12))
    light_dirs = to_light / distance.unsqueeze(-1)
    ndotl = (normals.view(*normals.shape[:2], 1, 1, 3) * light_dirs).sum(dim=-1)
    visibility = torch.ones_like(ndotl)
    shadow_origin = hit + normals * scene.ray_epsilon * 2.0
    for sphere_index in range(scene.sphere_centers.shape[0]):
        center = scene.sphere_centers[sphere_index]
        radius = scene.sphere_radii[sphere_index]
        oc = shadow_origin.view(*shadow_origin.shape[:2], 1, 1, 3) - center
        b = (oc * light_dirs).sum(dim=-1)
        c = (oc * oc).sum(dim=-1) - radius.square()
        discriminant = b.square() - c
        root = torch.sqrt(torch.clamp(discriminant, min=0.0))
        near = -b - root
        far = -b + root
        t_hit = torch.where(near > scene.ray_epsilon * 2.0, near, far)
        valid = (discriminant > 0.0) & (t_hit > scene.ray_epsilon * 2.0)
        softness = 0.04 + 0.015 * distance
        occluded = torch.sigmoid((distance - t_hit) / softness) * valid.to(albedo.dtype)
        visibility = visibility * (1.0 - occluded)
    lambert = torch.clamp(ndotl, min=0.0)
    light_sum = (
        visibility
        * lambert
        / (distance_sq + 0.20)
    )
    light_sum = light_sum.mean(dim=-1)
    light_sum = (light_sum * scene.light_intensities.view(1, 1, -1)).sum(dim=-1)
    return albedo * (scene.ambient + light_sum.unsqueeze(-1))


def _render_tile_direct(
    scene: RenderScene,
    camera: CameraSpec,
    full_height: int,
    full_width: int,
    *,
    x_offset: int,
    y_offset: int,
    tile_width: int,
    tile_height: int,
    shadow_samples: int,
    seed: int,
    x_pixel_scale: float = 1.0,
    y_pixel_scale: float = 1.0,
) -> torch.Tensor:
    origins, directions = camera_rays(
        camera,
        full_height,
        full_width,
        scene.background.dtype,
        scene.background.device,
        x_offset=x_offset,
        y_offset=y_offset,
        tile_width=tile_width,
        tile_height=tile_height,
        x_pixel_scale=x_pixel_scale,
        y_pixel_scale=y_pixel_scale,
    )
    hit, normals, albedo, active = _nearest_hit(scene, origins, directions)
    color = _direct_lighting(scene, hit, normals, albedo, seed, shadow_samples)
    background = scene.background.view(1, 1, 3).expand_as(color)
    return torch.where(active.unsqueeze(-1), color, background).clamp_min(0.0)


def render_image(
    scene: RenderScene,
    camera: CameraSpec,
    target: TargetImageSpec,
    config: RenderConfig,
    *,
    seed_offset: int = 0,
) -> torch.Tensor:
    config.validate()
    if target.width <= 0 or target.height <= 0:
        fail("E_PARA", "target dimensions must be positive", "target")
    factor = config.downsample_factor
    render_height = max(1, target.height // factor)
    render_width = max(1, target.width // factor)
    seed = int(config.base_seed) + int(seed_offset)
    image = _render_tile_direct(
        scene,
        camera,
        target.height,
        target.width,
        x_offset=0,
        y_offset=0,
        tile_width=render_width,
        tile_height=render_height,
        x_pixel_scale=target.width / render_width,
        y_pixel_scale=target.height / render_height,
        shadow_samples=config.shadow_samples,
        seed=seed,
    )
    if factor > 1:
        image = F.interpolate(
            image.permute(2, 0, 1).unsqueeze(0),
            size=(target.height, target.width),
            mode="bilinear",
            align_corners=False,
        ).squeeze(0).permute(1, 2, 0)
    return image


def render_tile(
    scene: RenderScene,
    camera: CameraSpec,
    target: TargetImageSpec,
    *,
    x_offset: int,
    y_offset: int,
    tile_width: int,
    tile_height: int,
    shadow_samples: int,
    seed: int,
) -> torch.Tensor:
    if not (1 <= shadow_samples <= 64):
        fail(
            "E_PARA",
            "shadow_samples must be in [1, 64]",
            "shadow_samples",
            value=shadow_samples,
        )
    return _render_tile_direct(
        scene,
        camera,
        target.height,
        target.width,
        x_offset=x_offset,
        y_offset=y_offset,
        tile_width=tile_width,
        tile_height=tile_height,
        shadow_samples=shadow_samples,
        seed=seed,
    )

