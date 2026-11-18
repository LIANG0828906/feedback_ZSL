"""CPU soft rasterizer, paper shading, and sampled cutout projection."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .procedural import (
    LayerTexture,
    SceneTextures,
    bilinear_sample,
    sample_layer_uv,
)
from .scene import Layer, Scene, project_point

EPSILON = 1e-12


@dataclass(frozen=True)
class ProjectionMap:
    inner_layer_index: int
    outer_layer_index: int
    air_gap_mm: float
    sample_density: int
    sample_count: int
    kernel_radius_inner_mm: float
    penumbra_target_mm: float
    visibility: np.ndarray

    def lookup(self, u: np.ndarray, v: np.ndarray) -> np.ndarray:
        u = np.asarray(u, dtype=np.float64)
        v = np.asarray(v, dtype=np.float64)
        inside = (u >= 0.0) & (u <= 1.0) & (v >= 0.0) & (v <= 1.0)
        sampled = bilinear_sample(self.visibility, u, v)
        return np.where(inside, np.clip(sampled, 0.0, 1.0), 1.0)


@dataclass(frozen=True)
class PreparedScene:
    scene: Scene
    textures: SceneTextures
    projections: dict[int, ProjectionMap]


@dataclass(frozen=True)
class LayerBlockMetrics:
    fragment_samples: int = 0
    paper_fragment_samples: int = 0
    hole_fragment_samples: int = 0
    transmittance_sum: float = 0.0

    def plus(self, other: "LayerBlockMetrics") -> "LayerBlockMetrics":
        return LayerBlockMetrics(
            fragment_samples=self.fragment_samples + other.fragment_samples,
            paper_fragment_samples=self.paper_fragment_samples
            + other.paper_fragment_samples,
            hole_fragment_samples=self.hole_fragment_samples
            + other.hole_fragment_samples,
            transmittance_sum=self.transmittance_sum + other.transmittance_sum,
        )


@dataclass(frozen=True)
class BlockRender:
    block_index: int
    x: int
    y: int
    width: int
    height: int
    rgba: np.ndarray
    optical_depth: np.ndarray
    layer_metrics: tuple[LayerBlockMetrics, ...]


def block_grid(camera) -> tuple[int, int]:
    columns = (camera.width + camera.block_width - 1) // camera.block_width
    rows = (camera.height + camera.block_height - 1) // camera.block_height
    return columns, rows


def block_bounds(scene: Scene, block_index: int) -> tuple[int, int, int, int]:
    columns, rows = block_grid(scene.camera)
    if block_index < 0 or block_index >= columns * rows:
        raise IndexError(f"block index {block_index} outside 0..{columns * rows - 1}")
    row, column = divmod(block_index, columns)
    x = column * scene.camera.block_width
    y = row * scene.camera.block_height
    width = min(scene.camera.block_width, scene.camera.width - x)
    height = min(scene.camera.block_height, scene.camera.height - y)
    return x, y, width, height


def _sample_offsets(samples_per_pixel: int) -> np.ndarray:
    if samples_per_pixel == 1:
        return np.asarray([[0.5, 0.5]], dtype=np.float64)
    offsets: list[tuple[float, float]] = []
    for index in range(1, samples_per_pixel + 1):
        h2 = 0.0
        factor = 0.5
        value = index
        while value > 0:
            h2 += factor * (value % 2)
            value //= 2
            factor *= 0.5
        h3 = 0.0
        factor = 1.0 / 3.0
        value = index
        while value > 0:
            h3 += factor * (value % 3)
            value //= 3
            factor /= 3.0
        offsets.append((h2, h3))
    return np.asarray(offsets, dtype=np.float64)


def _camera_basis(camera) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    forward = camera.target_mm - camera.position_mm
    forward = forward / np.linalg.norm(forward)
    right = np.cross(forward, camera.up)
    right = right / np.linalg.norm(right)
    true_up = np.cross(right, forward)
    true_up = true_up / np.linalg.norm(true_up)
    return forward, right, true_up


def _block_rays(
    scene: Scene, x: int, y: int, width: int, height: int
) -> tuple[np.ndarray, np.ndarray]:
    camera = scene.camera
    offsets = _sample_offsets(camera.samples_per_pixel)
    samples = offsets.shape[0]
    pixel_y, pixel_x = np.mgrid[y : y + height, x : x + width]
    pixel_x = pixel_x.ravel()
    pixel_y = pixel_y.ravel()
    repeated_x = np.repeat(pixel_x, samples)
    repeated_y = np.repeat(pixel_y, samples)
    tiled_offsets = np.tile(offsets, (len(pixel_x), 1))
    sub_x = (repeated_x + tiled_offsets[:, 0]) / camera.width
    sub_y = (repeated_y + tiled_offsets[:, 1]) / camera.height
    aspect = camera.width / camera.height
    tan_half = math.tan(math.radians(camera.fov_y_deg) * 0.5)
    forward, right, true_up = _camera_basis(camera)
    directions = (
        forward[None, :]
        + ((2.0 * sub_x - 1.0) * aspect * tan_half)[:, None] * right[None, :]
        + ((1.0 - 2.0 * sub_y) * tan_half)[:, None] * true_up[None, :]
    )
    directions /= np.linalg.norm(directions, axis=1, keepdims=True)
    origins = np.broadcast_to(camera.position_mm, directions.shape).copy()
    return origins, directions


def _intersect_layer(
    origins: np.ndarray, directions: np.ndarray, layer: Layer
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    vertices = layer.mesh.vertices.copy()
    vertices[:, 2] += layer.world_z_mm
    ray_count = directions.shape[0]
    best_t = np.full(ray_count, np.inf, dtype=np.float64)
    bary_u = np.zeros(ray_count, dtype=np.float64)
    bary_v = np.zeros(ray_count, dtype=np.float64)
    best_triangle = np.full(ray_count, -1, dtype=np.int64)

    for triangle_index, triangle in enumerate(layer.mesh.triangles):
        v0 = vertices[triangle[0]]
        v1 = vertices[triangle[1]]
        v2 = vertices[triangle[2]]
        edge1 = v1 - v0
        edge2 = v2 - v0
        pvec = np.cross(directions, edge2[None, :])
        det = np.einsum("ij,j->i", pvec, edge1)
        valid = np.abs(det) > 1e-11
        inv_det = np.zeros_like(det)
        inv_det[valid] = 1.0 / det[valid]
        tvec = origins - v0[None, :]
        u = np.einsum("ij,ij->i", tvec, pvec) * inv_det
        qvec = np.cross(tvec, edge1[None, :])
        v = np.einsum("ij,ij->i", directions, qvec) * inv_det
        t = np.einsum("ij,j->i", qvec, edge2) * inv_det
        hit = (
            valid
            & (u >= -1e-9)
            & (v >= -1e-9)
            & (u + v <= 1.0 + 1e-9)
            & (t > 1e-7)
            & (t < best_t)
        )
        if np.any(hit):
            best_t[hit] = t[hit]
            bary_u[hit] = u[hit]
            bary_v[hit] = v[hit]
            best_triangle[hit] = triangle_index

    hit_mask = best_triangle >= 0
    points = origins + directions * np.where(hit_mask, best_t, 0.0)[:, None]
    uv = np.zeros((ray_count, 2), dtype=np.float64)
    for triangle_index in np.unique(best_triangle[hit_mask]):
        selected = best_triangle == triangle_index
        triangle = layer.mesh.triangles[triangle_index]
        u = bary_u[selected]
        v = bary_v[selected]
        w = 1.0 - u - v
        uv[selected] = (
            w[:, None] * layer.mesh.uvs[triangle[0]][None, :]
            + u[:, None] * layer.mesh.uvs[triangle[1]][None, :]
            + v[:, None] * layer.mesh.uvs[triangle[2]][None, :]
        )
    return hit_mask, points, uv, best_t


def _tone_map(radiance: np.ndarray) -> np.ndarray:
    radiance = np.maximum(radiance, 0.0)
    peak = np.max(radiance, axis=1, keepdims=True)
    mapped_peak = 1.0 - np.exp(-1.65 * peak)
    scale = np.divide(
        mapped_peak,
        peak,
        out=np.zeros_like(peak),
        where=peak > 1e-12,
    )
    return np.power(np.clip(radiance * scale, 0.0, 1.0), 1.0 / 2.2)


def _light_factor(scene: Scene, points: np.ndarray) -> np.ndarray:
    delta = scene.light.position_mm[None, :] - points
    distance_sq = np.einsum("ij,ij->i", delta, delta)
    distance = np.sqrt(np.maximum(distance_sq, 1e-12))
    reference = scene.light.reference_distance_mm
    irradiance = scene.light.intensity * (reference * reference) / np.maximum(
        distance_sq, 1e-8
    )
    cosine = np.abs(delta[:, 2]) / np.maximum(distance, 1e-12)
    angle_factor = 0.22 + 0.78 * np.power(np.clip(cosine, 0.0, 1.0), 1.35)
    return irradiance * angle_factor


def _build_projection_map(
    scene: Scene,
    inner_layer: Layer,
    outer_layer: Layer,
    inner_texture: LayerTexture,
) -> ProjectionMap:
    texture = scene.texture
    width, height = texture.projection_width, texture.projection_height
    xmin, xmax, ymin, ymax = outer_layer.mesh.bounds
    xs = xmin + (np.arange(width, dtype=np.float64) + 0.5) * (xmax - xmin) / width
    ys = ymin + (np.arange(height, dtype=np.float64) + 0.5) * (ymax - ymin) / height
    grid_x, grid_y = np.meshgrid(xs, ys)
    light = scene.light.position_mm
    outer_z = outer_layer.world_z_mm
    inner_z = inner_layer.world_z_mm
    dz_outer = outer_z - float(light[2])
    if abs(dz_outer) <= 1e-9 or inner_z <= float(light[2]):
        raise ValueError("invalid layer ordering for projection")
    ratio = (inner_z - float(light[2])) / dz_outer
    inner_x = light[0] + ratio * (grid_x - light[0])
    inner_y = light[1] + ratio * (grid_y - light[1])
    ixmin, ixmax, iymin, iymax = inner_layer.mesh.bounds
    inner_u = (inner_x - ixmin) / max(ixmax - ixmin, 1e-12)
    inner_v = (inner_y - iymin) / max(iymax - iymin, 1e-12)

    gap = max(0.0, outer_z - inner_z)
    density = max(1, outer_layer.shadow_samples_per_axis)
    radius = (
        texture.penumbra_base_mm + texture.penumbra_per_gap_mm * gap
    )
    magnification = (outer_z - float(light[2])) / max(inner_z - float(light[2]), 1e-9)
    offsets = np.linspace(-radius, radius, density, dtype=np.float64)
    visibility = np.zeros((height, width), dtype=np.float64)
    inner_width = max(ixmax - ixmin, 1e-12)
    inner_height = max(iymax - iymin, 1e-12)
    for offset_y in offsets:
        for offset_x in offsets:
            sample_u = inner_u + offset_x / inner_width
            sample_v = inner_v + offset_y / inner_height
            samples = bilinear_sample(inner_texture.holes, sample_u, sample_v).astype(
                np.float64
            )
            outside = (
                (sample_u < 0.0)
                | (sample_u > 1.0)
                | (sample_v < 0.0)
                | (sample_v > 1.0)
            )
            visibility += np.where(outside, 1.0, samples)
    visibility /= float(density * density)
    return ProjectionMap(
        inner_layer_index=inner_layer.layer_index,
        outer_layer_index=outer_layer.layer_index,
        air_gap_mm=gap,
        sample_density=density,
        sample_count=int(width * height * density * density),
        kernel_radius_inner_mm=radius,
        penumbra_target_mm=2.0 * radius * magnification,
        visibility=visibility.astype(np.float32),
    )


def prepare_scene(scene: Scene, textures: SceneTextures | None = None) -> PreparedScene:
    if textures is None:
        from .procedural import generate_textures

        textures = generate_textures(scene)
    projections: dict[int, ProjectionMap] = {}
    ordered = scene.layers_inner_to_outer
    for outer, inner in zip(ordered[1:], ordered[:-1]):
        projections[outer.layer_index] = _build_projection_map(
            scene,
            inner,
            outer,
            textures.for_layer(inner.layer_index),
        )
    return PreparedScene(scene=scene, textures=textures, projections=projections)


def _render_block_impl(
    prepared: PreparedScene,
    block_index: int,
) -> BlockRender:
    scene = prepared.scene
    x, y, width, height = block_bounds(scene, block_index)
    origins, directions = _block_rays(scene, x, y, width, height)
    sample_count = origins.shape[0]
    pixel_count = width * height
    samples_per_pixel = scene.camera.samples_per_pixel
    ordered_layers = scene.layers_inner_to_outer

    hit_masks: list[np.ndarray] = []
    hit_points: list[np.ndarray] = []
    hit_uvs: list[np.ndarray] = []
    layer_colors: list[np.ndarray] = []
    layer_alphas: list[np.ndarray] = []
    optical_depth = np.zeros(sample_count, dtype=np.float64)
    layer_metrics: list[LayerBlockMetrics] = []

    for layer in ordered_layers:
        layer_texture = prepared.textures.for_layer(layer.layer_index)
        hit, points, uv, _ = _intersect_layer(origins, directions, layer)
        fibers = bilinear_sample(layer_texture.fibers, uv[:, 0], uv[:, 1])
        holes = bilinear_sample(
            layer_texture.holes.astype(np.float64), uv[:, 0], uv[:, 1]
        )
        paper_coverage = np.clip(1.0 - holes, 0.0, 1.0)
        tau = np.clip(
            layer.transmittance * (0.82 + 0.34 * fibers),
            0.02,
            0.985,
        )
        scatter = np.clip(
            np.asarray(layer.scatter_color, dtype=np.float64)[None, :]
            * (0.86 + 0.26 * fibers)[:, None],
            0.0,
            1.0,
        )
        light_factor = _light_factor(scene, points)
        projection = prepared.projections.get(layer.layer_index)
        if projection is None:
            visibility = np.ones(sample_count, dtype=np.float64)
        else:
            visibility = projection.lookup(uv[:, 0], uv[:, 1])

        transmitted = (
            tau
            * light_factor
            * (0.17 + 1.08 * visibility)
        )
        diffuse = 0.088 * (0.42 + 0.58 * np.minimum(light_factor, 1.5))
        light_tint = np.asarray(scene.light.color, dtype=np.float64)[None, :]
        radiance = scatter * (
            transmitted[:, None] * light_tint + diffuse[:, None]
        )
        mapped = _tone_map(radiance)
        alpha_paper = np.clip(0.17 + 0.58 * (1.0 - tau), 0.12, 0.74)
        alpha = alpha_paper * paper_coverage
        color = mapped * paper_coverage[:, None]
        color[~hit] = 0.0
        alpha[~hit] = 0.0

        hit_masks.append(hit)
        hit_points.append(points)
        hit_uvs.append(uv)
        layer_colors.append(color)
        layer_alphas.append(alpha)
        optical_depth += np.where(
            hit,
            -np.log(np.clip(tau, 1e-6, 1.0)) * paper_coverage,
            0.0,
        )

        paper = hit & (paper_coverage >= 0.5)
        hole = hit & ~paper
        layer_metrics.append(
            LayerBlockMetrics(
                fragment_samples=int(np.count_nonzero(hit)),
                paper_fragment_samples=int(np.count_nonzero(paper)),
                hole_fragment_samples=int(np.count_nonzero(hole)),
                transmittance_sum=float(np.sum(tau[paper], dtype=np.float64)),
            )
        )

    premultiplied = np.zeros((sample_count, 4), dtype=np.float64)
    for color, alpha in zip(layer_colors, layer_alphas):
        premultiplied[:, :3] = (
            color * alpha[:, None]
            + (1.0 - alpha[:, None]) * premultiplied[:, :3]
        )
        premultiplied[:, 3] = alpha + (1.0 - alpha) * premultiplied[:, 3]
    premultiplied = np.clip(premultiplied, 0.0, 1.0)

    premult_pixels = premultiplied.reshape(pixel_count, samples_per_pixel, 4).mean(
        axis=1
    )
    depth_pixels = optical_depth.reshape(pixel_count, samples_per_pixel).mean(axis=1)
    rgba = np.zeros((height, width, 4), dtype=np.uint8)
    rgba[:, :, :3] = np.clip(
        np.rint(premult_pixels[:, :3] * 255.0), 0, 255
    ).astype(np.uint8).reshape(height, width, 3)
    rgba[:, :, 3] = np.clip(
        np.rint(premult_pixels[:, 3] * 255.0), 0, 255
    ).astype(np.uint8).reshape(height, width)
    depth = depth_pixels.astype("<f4").reshape(height, width)
    return BlockRender(
        block_index=block_index,
        x=x,
        y=y,
        width=width,
        height=height,
        rgba=rgba,
        optical_depth=depth,
        layer_metrics=tuple(layer_metrics),
    )


def render_block(prepared: PreparedScene, block_index: int) -> BlockRender:
    return _render_block_impl(prepared, block_index)


def _sample_hole_at_inner(
    scene: Scene,
    inner: Layer,
    texture: LayerTexture,
    x_mm: np.ndarray,
    y_mm: np.ndarray,
    radius_mm: float,
    density: int,
) -> np.ndarray:
    u, v, _ = sample_layer_uv(inner, x_mm, y_mm)
    xmin, xmax, ymin, ymax = inner.mesh.bounds
    width = max(xmax - xmin, 1e-12)
    height = max(ymax - ymin, 1e-12)
    offsets = np.linspace(-radius_mm, radius_mm, density, dtype=np.float64)
    result = np.zeros_like(x_mm, dtype=np.float64)
    for offset_y in offsets:
        for offset_x in offsets:
            sample_u = u + offset_x / width
            sample_v = v + offset_y / height
            holes = bilinear_sample(texture.holes.astype(np.float64), sample_u, sample_v)
            outside = (
                (sample_u < 0.0)
                | (sample_u > 1.0)
                | (sample_v < 0.0)
                | (sample_v > 1.0)
            )
            result += np.where(outside, 1.0, holes)
    return result / float(density * density)


def build_penumbra_probes(
    scene: Scene,
    textures: SceneTextures,
    *,
    point_count: int = 241,
    span_mm: float = 60.0,
) -> list[dict]:
    ordered = scene.layers_inner_to_outer
    inner = ordered[0]
    inner_texture = textures.for_layer(0)
    height, width = inner_texture.holes.shape
    x0, x1, y0, y1 = inner.mesh.bounds
    target_u = 0.46
    target_v = 0.54
    grid_y, grid_x = np.mgrid[0:height, 0:width]
    holes = inner_texture.holes.astype(bool)
    distance = (grid_x / max(width - 1, 1) - target_u) ** 2 + (
        grid_y / max(height - 1, 1) - target_v
    ) ** 2
    candidates = np.argwhere(holes)
    if len(candidates) == 0:
        return []
    nearest = candidates[int(np.argmin(distance[holes]))]
    source_u = (nearest[1] + 0.5) / width
    source_v = (nearest[0] + 0.5) / height
    source_x = x0 + source_u * (x1 - x0)
    source_y = y0 + source_v * (y1 - y0)
    source = np.asarray([source_x, source_y, inner.world_z_mm], dtype=np.float64)
    light = scene.light.position_mm
    probes: list[dict] = []
    for outer in ordered[1:]:
        projected = project_point(light, source, outer.world_z_mm)
        if projected is None:
            continue
        gap = max(0.0, outer.world_z_mm - inner.world_z_mm)
        radius = (
            scene.texture.penumbra_base_mm
            + scene.texture.penumbra_per_gap_mm * gap
        )
        density = outer.shadow_samples_per_axis
        distances = np.linspace(0.0, span_mm, point_count, dtype=np.float64)
        target_points = np.column_stack(
            (
                projected[0] + distances,
                np.full_like(distances, projected[1]),
                np.full_like(distances, outer.world_z_mm),
            )
        )
        dz_outer = outer.world_z_mm - float(light[2])
        dz_inner = inner.world_z_mm - float(light[2])
        ratio = dz_inner / dz_outer
        inner_points = light[None, :] + ratio * (target_points - light[None, :])
        visibility = _sample_hole_at_inner(
            scene,
            inner,
            inner_texture,
            inner_points[:, 0],
            inner_points[:, 1],
            radius,
            density,
        )
        smoothed = visibility.copy()
        if len(smoothed) >= 5:
            kernel = np.asarray([1, 2, 3, 2, 1], dtype=np.float64)
            kernel /= kernel.sum()
            smoothed = np.convolve(smoothed, kernel, mode="same")
        last_high: int | None = None
        first_low: int | None = None
        for point_index, value in enumerate(smoothed):
            if value >= 0.95:
                last_high = point_index
            if last_high is not None and value <= 0.05:
                first_low = point_index
                break
        if last_high is None:
            width_mm = 0.0
        elif first_low is None:
            width_mm = float(span_mm)
        else:
            width_mm = float(distances[first_low] - distances[last_high])
        if last_high is None:
            report_start = 0
            report_stop = point_count - 1
        else:
            edge_low = first_low if first_low is not None else last_high + 8
            margin = max(4, (edge_low - last_high) * 2)
            report_start = max(0, last_high - margin)
            report_stop = min(point_count - 1, edge_low + margin)
        report_indices = np.linspace(
            report_start, report_stop, 9, dtype=np.int64
        )
        probes.append(
            {
                "source_layer": inner.layer_index,
                "target_layer": outer.layer_index,
                "air_gap_mm": round(gap, 6),
                "sample_density": density,
                "kernel_radius_inner_mm": round(radius, 6),
                "half_penumbra_width_mm": round(width_mm, 6),
                "sample_offsets_mm": [
                    round(float(distances[index]), 6) for index in report_indices
                ],
                "hole_visibility": [
                    round(float(visibility[index]), 6) for index in report_indices
                ],
            }
        )
    return probes
