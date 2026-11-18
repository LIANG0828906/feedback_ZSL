from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .geometry import (
    GeometryContext,
    LeadBar,
)
from .scene import Pane, Scene
from .shadow import wall_linear_radiance
from .texture import sample_texture

RENDERER_VERSION = "glass-verify-raster-1"


@dataclass
class ChunkResult:
    index: int
    x0: int
    y0: int
    width: int
    height: int
    rgb: np.ndarray
    labels: np.ndarray
    metrics: dict[str, Any]


def _unit_rows(vectors: np.ndarray) -> np.ndarray:
    lengths = np.linalg.norm(vectors, axis=1)
    safe = np.where(lengths > 1.0e-12, lengths, 1.0)
    return vectors / safe[:, None]


def camera_rays(
    scene: Scene,
    x0: int,
    y0: int,
    width: int,
    height: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    camera = scene.camera
    forward = _unit_rows((camera.target - camera.position)[None, :])[0]
    right = _unit_rows(np.cross(forward, camera.up)[None, :])[0]
    true_up = np.cross(right, forward)
    aspect = camera.width / camera.height
    tan_half = math.tan(math.radians(camera.fov_y_deg) * 0.5)
    supersample = scene.render.supersample
    origins: list[np.ndarray] = []
    directions: list[np.ndarray] = []
    xs = np.arange(x0, x0 + width, dtype=np.float64)
    ys = np.arange(y0, y0 + height, dtype=np.float64)
    for sample_y in range(supersample):
        for sample_x in range(supersample):
            offset_x = (sample_x + 0.5) / supersample - 0.5
            offset_y = (sample_y + 0.5) / supersample - 0.5
            px = xs[None, :] + 0.5 + offset_x
            py = ys[:, None] + 0.5 + offset_y
            sx = (px / camera.width * 2.0 - 1.0) * tan_half * aspect
            sy = (1.0 - py / camera.height * 2.0) * tan_half
            sx_grid = np.broadcast_to(sx, (height, width))
            sy_grid = np.broadcast_to(sy, (height, width))
            ray_dirs = (
                forward[None, None, :]
                + sx_grid[:, :, None] * right[None, None, :]
                + sy_grid[:, :, None] * true_up[None, None, :]
            )
            origins.append(np.broadcast_to(camera.position, (height * width, 3)))
            directions.append(ray_dirs.reshape(-1, 3))
    return (
        np.concatenate(origins, axis=0),
        _unit_rows(np.concatenate(directions, axis=0)),
        np.asarray([supersample * supersample, height * width], dtype=np.int64),
    )


def _intersect_triangle_batch(
    origins: np.ndarray,
    directions: np.ndarray,
    first: np.ndarray,
    second: np.ndarray,
    third: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    edge1 = second - first
    edge2 = third - first
    pvec = np.cross(directions, edge2[None, :])
    determinant = pvec @ edge1
    valid = np.abs(determinant) > 1.0e-12
    inv = np.zeros_like(determinant)
    inv[valid] = 1.0 / determinant[valid]
    tvec = origins - first[None, :]
    u = (tvec * pvec).sum(axis=1) * inv
    qvec = np.cross(tvec, edge1[None, :])
    v = (directions * qvec).sum(axis=1) * inv
    distance = (qvec * edge2[None, :]).sum(axis=1) * inv
    hit = valid & (u >= -1.0e-9) & (v >= -1.0e-9) & (u + v <= 1.0 + 1.0e-9) & (distance > 1.0e-8)
    return distance, hit


@dataclass
class GlassHits:
    ray_index: np.ndarray
    distance: np.ndarray
    pane_index: np.ndarray
    triangle_index: np.ndarray


def intersect_glass(scene: Scene, origins: np.ndarray, directions: np.ndarray) -> GlassHits:
    ray_parts: list[np.ndarray] = []
    distance_parts: list[np.ndarray] = []
    pane_parts: list[np.ndarray] = []
    triangle_parts: list[np.ndarray] = []
    ray_numbers = np.arange(origins.shape[0], dtype=np.int64)
    for pane_index, pane in enumerate(scene.panes):
        for triangle_index, triangle in enumerate(pane.triangles):
            distance, hit = _intersect_triangle_batch(
                origins,
                directions,
                pane.vertices[triangle[0]],
                pane.vertices[triangle[1]],
                pane.vertices[triangle[2]],
            )
            selected = np.flatnonzero(hit & (distance < scene.camera.far))
            if selected.size:
                ray_parts.append(ray_numbers[selected])
                distance_parts.append(distance[selected])
                pane_parts.append(np.full(selected.size, pane_index, dtype=np.int64))
                triangle_parts.append(np.full(selected.size, triangle_index, dtype=np.int64))
    if not ray_parts:
        empty_i = np.zeros(0, dtype=np.int64)
        empty_f = np.zeros(0, dtype=np.float64)
        return GlassHits(empty_i, empty_f, empty_i, empty_i)
    return GlassHits(
        np.concatenate(ray_parts),
        np.concatenate(distance_parts),
        np.concatenate(pane_parts),
        np.concatenate(triangle_parts),
    )


def _intersect_lead_prism(
    origins: np.ndarray,
    directions: np.ndarray,
    bar: LeadBar,
) -> np.ndarray:
    local_origin = np.column_stack(
        (
            (origins - bar.center) @ bar.axis_e,
            (origins - bar.center) @ bar.axis_m,
            (origins - bar.center) @ bar.axis_n,
        )
    )
    local_direction = np.column_stack(
        (
            directions @ bar.axis_e,
            directions @ bar.axis_m,
            directions @ bar.axis_n,
        )
    )
    half = np.asarray([bar.half_length, bar.half_width, bar.half_depth], dtype=np.float64)
    enter = np.full(origins.shape[0], -np.inf, dtype=np.float64)
    exit_ = np.full(origins.shape[0], np.inf, dtype=np.float64)
    valid = np.ones(origins.shape[0], dtype=bool)
    for axis in range(3):
        direction = local_direction[:, axis]
        origin = local_origin[:, axis]
        parallel = np.abs(direction) <= 1.0e-12
        inside = np.abs(origin) <= half[axis]
        valid &= ~(parallel & ~inside)
        safe = np.where(parallel, 1.0, direction)
        t1 = (-half[axis] - origin) / safe
        t2 = (half[axis] - origin) / safe
        low = np.minimum(t1, t2)
        high = np.maximum(t1, t2)
        enter = np.where(~parallel, np.maximum(enter, low), enter)
        exit_ = np.where(~parallel, np.minimum(exit_, high), exit_)
    valid &= enter <= exit_
    valid &= exit_ > 1.0e-8
    enter = np.where(enter > 1.0e-8, enter, exit_)
    valid &= enter > 1.0e-8
    return np.where(valid, enter, np.inf)


def intersect_lead(
    context: GeometryContext,
    origins: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    nearest_t = np.full(origins.shape[0], np.inf, dtype=np.float64)
    nearest_bar = np.full(origins.shape[0], -1, dtype=np.int64)
    for bar_index, bar in enumerate(context.lead_bars):
        distance = _intersect_lead_prism(origins, directions, bar)
        better = distance < nearest_t
        nearest_t[better] = distance[better]
        nearest_bar[better] = bar_index
    return nearest_t, nearest_bar


def intersect_walls(
    scene: Scene,
    origins: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    nearest_t = np.full(origins.shape[0], np.inf, dtype=np.float64)
    nearest_wall = np.full(origins.shape[0], -1, dtype=np.int64)
    for wall_index, wall in enumerate(scene.walls):
        denominator = directions @ wall.normal
        valid = np.abs(denominator) > 1.0e-10
        safe = np.where(valid, denominator, 1.0)
        distance = ((wall.point - origins) @ wall.normal) / safe
        points = origins + distance[:, None] * directions
        relative = points - wall.point
        u = relative @ wall.axis_u
        v = relative @ wall.axis_v
        hit = (
            valid
            & (distance > scene.camera.near)
            & (distance < scene.camera.far)
            & (np.abs(u) <= wall.extent_u * 0.5 + 1.0e-9)
            & (np.abs(v) <= wall.extent_v * 0.5 + 1.0e-9)
        )
        better = hit & (distance < nearest_t)
        nearest_t[better] = distance[better]
        nearest_wall[better] = wall_index
    return nearest_t, nearest_wall


def _pane_uv(scene: Scene, pane: Pane, points: np.ndarray) -> np.ndarray:
    relative = points - scene.window.origin
    u = relative @ scene.window.axis_u
    v = relative @ scene.window.axis_v
    extent = np.maximum(pane.uv_max - pane.uv_min, 1.0e-9)
    return np.column_stack(
        ((u - pane.uv_min[0]) / extent[0], (v - pane.uv_min[1]) / extent[1])
    )


def _compose_transmission_reflection(
    transmission: np.ndarray,
    reflection: np.ndarray,
    order: str,
) -> np.ndarray:
    transmission = np.clip(transmission, 0.0, 1.0)
    reflection = np.clip(reflection, 0.0, 1.0)
    if order == "transmission_then_reflection":
        return transmission + reflection * (1.0 - transmission)
    return reflection + transmission * (1.0 - reflection)


def _shade_glass_hits(
    context: GeometryContext,
    origins: np.ndarray,
    directions: np.ndarray,
    hits: GlassHits,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    scene = context.scene
    count = hits.ray_index.shape[0]
    transmission = np.zeros((count, 3), dtype=np.float64)
    reflection = np.zeros((count, 3), dtype=np.float64)
    composed = np.zeros((count, 3), dtype=np.float64)
    if count == 0:
        return composed, transmission, reflection
    points = origins[hits.ray_index] + hits.distance[:, None] * directions[hits.ray_index]
    pane_indices = hits.pane_index
    reflect_base = scene.render.reflection_tint * (
        scene.render.ambient_light + 0.20 * scene.light.irradiance
    )
    for pane_index, pane in enumerate(scene.panes):
        mask = pane_indices == pane_index
        if not np.any(mask):
            continue
        subset_points = points[mask]
        subset_directions = directions[hits.ray_index[mask]]
        uv = _pane_uv(scene, pane, subset_points)
        noise = sample_texture(context.textures[pane_index], uv)
        variation = context.pane_variations[pane_index]
        transmission_texture = np.clip(
            1.0
            + scene.texture.transmission_strength * (2.0 * noise - 1.0),
            0.30,
            1.70,
        ) * variation
        reflection_texture = np.clip(
            1.0
            + scene.texture.reflection_strength * (2.0 * noise - 1.0),
            0.30,
            1.70,
        ) * variation
        color_band = np.clip(
            pane.transmission_color[None, :] * transmission_texture[:, None], 0.0, 1.0
        )
        direct_shape = np.maximum(0.0, float(pane.normal @ scene.light.direction))
        transmission_energy = (
            scene.light.irradiance[None, :]
            * (0.58 + 0.42 * direct_shape)
            * pane.transmittance
            * color_band
        )
        ambient_transmission = (
            scene.render.ambient_light[None, :]
            * 0.34
            * pane.transmittance
            * color_band
        )
        trans = np.clip(transmission_energy + ambient_transmission, 0.0, 1.0)

        cos_view = np.abs(subset_directions @ pane.normal)
        cos_view = np.clip(cos_view, 0.0, 1.0)
        reflectance0 = np.clip(
            pane.glaze_reflectance * reflection_texture, 0.0, 1.0
        )
        fresnel = reflectance0 + (1.0 - reflectance0) * np.power(1.0 - cos_view, 5.0)
        fresnel = np.minimum(fresnel, scene.render.grazing_reflectance_cap)
        refl = fresnel[:, None] * reflect_base[None, :]
        # Grazing reflection is deliberately achromatic and deterministic. This
        # removes the spurious colour speckle from naively summed layers.
        grazing = np.clip((0.30 - cos_view) / 0.30, 0.0, 1.0)
        luminance = refl @ np.asarray([0.2126, 0.7152, 0.0722], dtype=np.float64)
        refl = refl * (1.0 - 0.78 * grazing[:, None]) + luminance[:, None] * (
            0.78 * grazing[:, None]
        )
        refl = np.clip(refl, 0.0, 1.0)
        transmission[mask] = trans
        reflection[mask] = refl
        composed[mask] = _compose_transmission_reflection(
            trans, refl, scene.render.composite_order
        )
    return composed, transmission, reflection


def _lead_linear_radiance(scene: Scene, count: int) -> np.ndarray:
    base = np.asarray([0.040, 0.045, 0.052], dtype=np.float64)
    angle = abs(float(scene.window.normal @ scene.light.direction))
    direct = scene.light.irradiance * 0.20 * angle * base
    ambient = scene.render.ambient_light * base
    return np.broadcast_to(direct + ambient, (count, 3))


def _mode_labels(labels: np.ndarray, sample_count: int, pixel_count: int) -> np.ndarray:
    reshaped = labels.reshape(sample_count, pixel_count)
    best_count = np.full(pixel_count, -1, dtype=np.int64)
    best_label = np.zeros(pixel_count, dtype=np.uint16)
    for label in np.unique(reshaped):
        count = np.sum(reshaped == label, axis=0)
        better = (count > best_count) | ((count == best_count) & (label < best_label))
        best_count[better] = count[better]
        best_label[better] = np.uint16(label)
    return best_label


def _initial_metrics(scene: Scene) -> dict[str, Any]:
    return {
        "fragment_hits": {str(sector): 0 for sector in scene.sector_ids},
        "visible_pixels": {str(sector): 0 for sector in scene.sector_ids},
        "pane_fragment_hits": {
            f"{pane.sector_id}:{pane.pane_id}": 0 for pane in scene.panes
        },
        "pane_transmittance_sum": {
            f"{pane.sector_id}:{pane.pane_id}": 0.0 for pane in scene.panes
        },
        "pane_transmission_energy": {
            f"{pane.sector_id}:{pane.pane_id}": 0.0 for pane in scene.panes
        },
        "pane_reflection_energy": {
            f"{pane.sector_id}:{pane.pane_id}": 0.0 for pane in scene.panes
        },
        "glass_rays": 0,
        "lead_rays": 0,
        "wall_rays": 0,
        "background_rays": 0,
        "shadow_wall_points": 0,
        "shadow_edge_probes": 0,
        "shadow_offset_samples": 0,
        "soft_shadow_samples_per_edge": int(scene.projection.samples_per_axis ** 2),
    }


def render_rays(
    context: GeometryContext,
    origins: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, dict[str, Any]]:
    scene = context.scene
    ray_count = origins.shape[0]
    colors = np.broadcast_to(scene.render.background, (ray_count, 3)).copy()
    labels = np.zeros(ray_count, dtype=np.uint16)
    metrics = _initial_metrics(scene)

    hits = intersect_glass(scene, origins, directions)
    lead_t, lead_bar_index = intersect_lead(context, origins, directions)
    wall_t, wall_index = intersect_walls(scene, origins, directions)

    nearest_glass_t = np.full(ray_count, np.inf, dtype=np.float64)
    nearest_glass_pane = np.full(ray_count, -1, dtype=np.int64)
    if hits.ray_index.size:
        order = np.lexsort(
            (
                hits.triangle_index,
                hits.pane_index,
                hits.distance,
            )
        )
        ray_sorted = hits.ray_index[order]
        # Global distance sorting interleaves rays, so run-length detection is
        # wrong here. np.unique's first index is the nearest hit for each ray.
        _, first_indices = np.unique(ray_sorted, return_index=True)
        selected = order[first_indices]
        nearest_glass_t[hits.ray_index[selected]] = hits.distance[selected]
        nearest_glass_pane[hits.ray_index[selected]] = hits.pane_index[selected]

    glass_visible = (
        np.isfinite(nearest_glass_t)
        & (nearest_glass_t < lead_t)
        & (nearest_glass_t < wall_t)
    )
    lead_visible = np.isfinite(lead_t) & (lead_t < nearest_glass_t) & (lead_t < wall_t)
    wall_visible = np.isfinite(wall_t) & (wall_t < nearest_glass_t) & (wall_t < lead_t)
    background_visible = ~(glass_visible | lead_visible | wall_visible)

    metrics["glass_rays"] = int(np.count_nonzero(glass_visible))
    metrics["lead_rays"] = int(np.count_nonzero(lead_visible))
    metrics["wall_rays"] = int(np.count_nonzero(wall_visible))
    metrics["background_rays"] = int(np.count_nonzero(background_visible))

    if np.any(wall_visible):
        for current_wall_index, wall in enumerate(scene.walls):
            mask = wall_visible & (wall_index == current_wall_index)
            if not np.any(mask):
                continue
            points = origins[mask] + wall_t[mask, None] * directions[mask]
            radiance, shadow_metrics = wall_linear_radiance(context, wall, points)
            colors[mask] = radiance
            metrics["shadow_wall_points"] += int(shadow_metrics["wall_points"])
            metrics["shadow_edge_probes"] += int(shadow_metrics["edge_probes"])
            metrics["shadow_offset_samples"] += int(shadow_metrics["sampled_offsets"])

    if np.any(lead_visible):
        colors[lead_visible] = _lead_linear_radiance(scene, int(np.count_nonzero(lead_visible)))
        labels[lead_visible] = 1

    if np.any(glass_visible):
        hit_on_glass_ray = glass_visible[hits.ray_index]
        glass_hit_indices = np.flatnonzero(hit_on_glass_ray)
        if glass_hit_indices.size:
            composed, transmission, reflection = _shade_glass_hits(
                context, origins, directions, hits
            )
            usable_hits = np.zeros(hits.ray_index.shape[0], dtype=bool)
            usable_hits[glass_hit_indices] = True
            layer_output = np.zeros((ray_count, 3), dtype=np.float64)
            if usable_hits.any():
                layer_indices = np.flatnonzero(usable_hits)
                sector_ids = np.asarray(
                    [scene.panes[hits.pane_index[i]].sector_id for i in layer_indices],
                    dtype=np.int64,
                )
                pane_ids = np.asarray(
                    [scene.panes[hits.pane_index[i]].pane_id for i in layer_indices],
                    dtype=np.int64,
                )
                layer_order = np.lexsort(
                    (
                        hits.triangle_index[layer_indices],
                        hits.distance[layer_indices],
                        pane_ids,
                        sector_ids,
                    )
                )
                ordered = layer_indices[layer_order]
                for hit_index in ordered:
                    ray_index = hits.ray_index[hit_index]
                    color = composed[hit_index]
                    layer_output[ray_index] = (
                        layer_output[ray_index]
                        + color
                        * (1.0 - layer_output[ray_index])
                    )
                colors[glass_visible] = layer_output[glass_visible]
            for pane_index, pane in enumerate(scene.panes):
                key = f"{pane.sector_id}:{pane.pane_id}"
                pane_mask = usable_hits & (hits.pane_index == pane_index)
                hit_count = int(np.count_nonzero(pane_mask))
                if hit_count:
                    metrics["pane_fragment_hits"][key] += hit_count
                    metrics["fragment_hits"][str(pane.sector_id)] += hit_count
                    metrics["pane_transmittance_sum"][key] += pane.transmittance * hit_count
                    metrics["pane_transmission_energy"][key] += float(
                        transmission[pane_mask].sum()
                    )
                    metrics["pane_reflection_energy"][key] += float(
                        reflection[pane_mask].sum()
                    )

        selected_panes = nearest_glass_pane[glass_visible]
        for pane_index in np.unique(selected_panes):
            pane = scene.panes[int(pane_index)]
            label = context.labels_by_key[(pane.sector_id, pane.pane_id)]
            pane_pixel_mask = glass_visible & (nearest_glass_pane == pane_index)
            labels[pane_pixel_mask] = np.uint16(label)
            metrics["visible_pixels"][str(pane.sector_id)] += int(
                np.count_nonzero(pane_pixel_mask)
            )

    return colors, labels, metrics


def _linear_to_gamma_u8(linear: np.ndarray, scene: Scene) -> np.ndarray:
    exposed = np.clip(linear * scene.render.exposure, 0.0, 1.0)
    encoded = np.sqrt(exposed)
    if scene.render.output_rounding == "truncate":
        return np.clip(encoded * 255.0, 0.0, 255.0).astype(np.uint8)
    return np.clip(np.floor(encoded * 255.0 + 0.5), 0.0, 255.0).astype(np.uint8)


def merge_metrics(target: dict[str, Any], source: dict[str, Any]) -> None:
    for key, value in source.items():
        if key == "soft_shadow_samples_per_edge":
            target[key] = value
        elif isinstance(value, dict):
            destination = target.setdefault(key, {})
            for sub_key, sub_value in value.items():
                destination[sub_key] = destination.get(sub_key, 0) + sub_value
        else:
            target[key] = target.get(key, 0) + value


def empty_metrics(scene: Scene) -> dict[str, Any]:
    return _initial_metrics(scene)


def chunk_count(scene: Scene) -> tuple[int, int, int]:
    chunk_width, chunk_height = scene.render.chunk_size
    columns = math.ceil(scene.camera.width / chunk_width)
    rows = math.ceil(scene.camera.height / chunk_height)
    return columns, rows, columns * rows


def chunk_bounds(scene: Scene, index: int) -> tuple[int, int, int, int]:
    columns, rows, total = chunk_count(scene)
    if not 0 <= index < total:
        raise IndexError(f"chunk index {index} outside [0,{total})")
    chunk_width, chunk_height = scene.render.chunk_size
    column = index % columns
    row = index // columns
    x0 = column * chunk_width
    y0 = row * chunk_height
    width = min(chunk_width, scene.camera.width - x0)
    height = min(chunk_height, scene.camera.height - y0)
    return x0, y0, width, height


def render_chunk(context: GeometryContext, index: int) -> ChunkResult:
    scene = context.scene
    x0, y0, width, height = chunk_bounds(scene, index)
    origins, directions, shape = camera_rays(scene, x0, y0, width, height)
    actual_samples, pixel_count = int(shape[0]), int(shape[1])
    linear, ray_labels, metrics = render_rays(context, origins, directions)
    mean_linear = linear.reshape(actual_samples, pixel_count, 3).mean(axis=0)
    rgb = _linear_to_gamma_u8(mean_linear, scene).reshape(height, width, 3)
    labels = _mode_labels(ray_labels, actual_samples, pixel_count).reshape(height, width)
    return ChunkResult(index, x0, y0, width, height, rgb, labels, metrics)
