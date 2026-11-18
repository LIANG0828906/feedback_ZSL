"""Block-based deterministic layered aquarium renderer."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .geometry import (
    first_hit,
    mesh_arrays,
    ray_mesh_intersections,
    sorted_shell_interval,
)
from .particles import ParticleTable, sample_modulation
from .projection import (
    build_projection_geometry,
    receiver_shadow_factors,
    water_shadow_factors,
)
from .scene import Component, Scene


def _normalize(vector: np.ndarray) -> np.ndarray:
    length = float(np.linalg.norm(vector))
    if length <= 1e-15:
        return vector
    return vector / length


def camera_rays(scene: Scene, angle_deg: float, x: np.ndarray, y: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    camera = scene.camera
    position = camera.position(angle_deg)
    forward = _normalize(camera.target - position)
    right = _normalize(np.cross(forward, np.asarray([0.0, 0.0, 1.0])))
    up = _normalize(np.cross(right, forward))
    aspect = scene.render.width / scene.render.height
    tan_half = math.tan(math.radians(camera.fov_deg) * 0.5)
    ndc_x = ((x + 0.5) / scene.render.width * 2.0 - 1.0) * tan_half * aspect
    ndc_y = (1.0 - (y + 0.5) / scene.render.height * 2.0) * tan_half
    directions = (
        forward[None, :]
        + ndc_x[:, None] * right[None, :]
        + ndc_y[:, None] * up[None, :]
    )
    norms = np.linalg.norm(directions, axis=1)
    directions = directions / np.maximum(norms[:, None], 1e-15)
    origins = np.broadcast_to(position, directions.shape).copy()
    return origins, directions


def _component_interval(
    origins: np.ndarray,
    directions: np.ndarray,
    component: Component,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    vertices, triangles = mesh_arrays(component.mesh)
    hits = ray_mesh_intersections(origins, directions, vertices, triangles)
    entry, exit_t = sorted_shell_interval(hits)
    valid = np.isfinite(entry) & np.isfinite(exit_t) & (exit_t > entry)
    entry_point = origins + np.where(valid, entry, 0.0)[:, None] * directions
    return entry, exit_t, valid, entry_point


def _radial_normals(points: np.ndarray) -> np.ndarray:
    normals = np.zeros_like(points)
    normals[:, 0] = points[:, 0]
    normals[:, 1] = points[:, 1]
    lengths = np.linalg.norm(normals, axis=1)
    return normals / np.maximum(lengths[:, None], 1e-15)


def _light_term(
    scene: Scene,
    points: np.ndarray,
    normals: np.ndarray | None = None,
) -> tuple[np.ndarray, list[np.ndarray], list[np.ndarray]]:
    term = np.full(len(points), scene.render.ambient, dtype=np.float64)
    colors: list[np.ndarray] = []
    distances: list[np.ndarray] = []
    for light in scene.lights:
        delta = light.position[None, :] - points
        distance = np.linalg.norm(delta, axis=1)
        direction = delta / np.maximum(distance[:, None], 1e-15)
        if normals is None:
            cosine = np.ones(len(points), dtype=np.float64)
        else:
            cosine = np.clip(np.sum(normals * direction, axis=1), 0.0, 1.0)
        contribution = (
            float(light.intensity)
            / (1.0 + 0.55 * distance * distance)
            * (0.18 + 0.82 * cosine)
        )
        term += contribution
        colors.append(light.color)
        distances.append(distance)
    return term, colors, distances


def _stable_pebble_index(points: np.ndarray, palette_size: int) -> np.ndarray:
    angle = np.arctan2(points[:, 1], points[:, 0])
    radius = np.hypot(points[:, 0], points[:, 1])
    cell = np.floor((angle + math.pi) * 23.0).astype(np.int64)
    ring = np.floor(radius * 37.0).astype(np.int64)
    mixed = ((cell * 2654435761) ^ (ring * 2246822519)) & 0x7FFFFFFF
    return mixed % palette_size


@dataclass
class BlockResult:
    rgba: np.ndarray
    ownership: np.ndarray
    layer_stats: dict[str, dict[str, float]]


class Renderer:
    def __init__(self, scene: Scene, particle_tables: dict[str, ParticleTable]):
        self.scene = scene
        self.particle_tables = particle_tables
        self.transparent_components: list[Component] = sorted(
            scene.components,
            key=lambda component: (0 if component.kind == "glass" else 1, component.layer_index),
        )
        self.projection_geometry = build_projection_geometry(scene)
        self._angles = scene.camera.angles()

    @property
    def angles(self) -> np.ndarray:
        return self._angles

    @property
    def blocks_per_angle(self) -> int:
        return self.scene.render.blocks_per_angle

    def block_rect(self, block_id: int) -> tuple[int, int, int, int]:
        render = self.scene.render
        by, bx = divmod(block_id, render.blocks_x)
        x0 = bx * render.block_width
        y0 = by * render.block_height
        x1 = min(x0 + render.block_width, render.width)
        y1 = min(y0 + render.block_height, render.height)
        return x0, y0, x1, y1

    def render_block(self, angle_index: int, block_id: int) -> BlockResult:
        scene = self.scene
        render = scene.render
        x0, y0, x1, y1 = self.block_rect(block_id)
        width = x1 - x0
        height = y1 - y0
        yy, xx = np.mgrid[y0:y1, x0:x1]
        pixel_x = xx.ravel().astype(np.float64)
        pixel_y = yy.ravel().astype(np.float64)
        origins, directions = camera_rays(scene, float(self.angles[angle_index]), pixel_x, pixel_y)
        count = origins.shape[0]
        pixel_indices = np.arange(count)

        intervals: dict[str, dict[str, np.ndarray]] = {}
        for component in self.transparent_components:
            entry, exit_t, valid, entry_point = _component_interval(origins, directions, component)
            intervals[component.component_id] = {
                "entry": entry,
                "exit": exit_t,
                "valid": valid,
                "point": entry_point,
            }

        fish_t = np.full((count, len(scene.fish)), np.inf, dtype=np.float64)
        fish_points = np.zeros((count, len(scene.fish), 3), dtype=np.float64)
        for index, fish in enumerate(scene.fish):
            vertices, triangles = mesh_arrays(fish.mesh)
            hits = ray_mesh_intersections(origins, directions, vertices, triangles)
            fish_t[:, index] = first_hit(hits)
            valid = np.isfinite(fish_t[:, index])
            fish_points[:, index, :] = origins + np.where(valid, fish_t[:, index], 0.0)[:, None] * directions

        receiver_vertices, receiver_triangles = mesh_arrays(scene.receiver.mesh)
        receiver_hits = ray_mesh_intersections(
            origins, directions, receiver_vertices, receiver_triangles
        )
        receiver_t = first_hit(receiver_hits)
        receiver_points = origins + np.where(np.isfinite(receiver_t), receiver_t, 0.0)[:, None] * directions

        nearest_fish_index = np.argmin(fish_t, axis=1)
        nearest_fish_t = fish_t[pixel_indices, nearest_fish_index]
        solid_t = np.minimum(nearest_fish_t, receiver_t)
        component_count = len(self.transparent_components)
        entries = np.empty((count, component_count), dtype=np.float64)
        trans = np.empty((count, component_count, 3), dtype=np.float64)
        emit = np.zeros((count, component_count, 3), dtype=np.float64)
        alpha = np.empty((count, component_count), dtype=np.float64)

        for component_index, component in enumerate(self.transparent_components):
            info = intervals[component.component_id]
            entry = info["entry"]
            exit_t = info["exit"]
            valid = info["valid"]
            entry_point = info["point"]
            entries[:, component_index] = np.where(valid, entry, np.inf)
            solid_distance = np.minimum(np.where(valid, exit_t, np.inf), solid_t)
            optical_segment = np.zeros(count, dtype=np.float64)
            np.subtract(solid_distance, entry, out=optical_segment, where=valid)
            optical_segment = np.maximum(optical_segment, 0.0)
            radial_normal = _radial_normals(entry_point)
            cosine = np.abs(np.sum(radial_normal * directions, axis=1))
            cosine = np.clip(cosine, 0.20, 1.0)
            if component.kind == "glass":
                thickness = float(component.thickness or 0.01)
                path = np.minimum(thickness / cosine, thickness * 5.0)
                base = np.clip(
                    float(component.transmission if component.transmission is not None else 1.0)
                    * component.tint,
                    1e-6,
                    1.0,
                )
                layer_trans = np.power(base[None, :], (path / max(thickness, 1e-6))[:, None])
                layer_alpha = np.clip(1.0 - np.mean(layer_trans, axis=1), 0.0, 1.0)
                f0 = ((component.ior - 1.0) / (component.ior + 1.0)) ** 2
                fresnel = f0 + (1.0 - f0) * np.power(1.0 - cosine, 5.0)
                light_total, _, _ = _light_term(scene, entry_point, radial_normal)
                glint = (
                    component.tint[None, :]
                    * light_total[:, None]
                    * fresnel[:, None]
                    * scene.render.reflection_strength
                    * 0.60
                )
                glint *= (0.35 + 0.65 * np.power(np.clip(cosine, 0.0, 1.0), 8.0))[:, None]
                ambient_glow = (
                    component.tint[None, :]
                    * scene.render.ambient
                    * 2.2
                    * layer_alpha[:, None]
                )
                layer_emit = np.minimum(glint + ambient_glow, layer_alpha[:, None])
            else:
                table = self.particle_tables[component.component_id]
                vertices = component.vertices
                radius = np.hypot(entry_point[:, 0], entry_point[:, 1])
                angle = np.arctan2(entry_point[:, 1], entry_point[:, 0])
                radial_all = np.hypot(vertices[:, 0], vertices[:, 1])
                angle_all = np.arctan2(vertices[:, 1], vertices[:, 0])
                zmin = float(vertices[:, 2].min())
                zmax = float(vertices[:, 2].max())
                a0 = float(angle_all.min())
                a1 = float(angle_all.max())
                r0 = float(radial_all.min())
                r1 = float(radial_all.max())
                u = np.clip((angle - a0) / max(a1 - a0, 1e-8), 0.0, 1.0)
                v = np.clip((entry_point[:, 2] - zmin) / max(zmax - zmin, 1e-8), 0.0, 1.0)
                modulation = sample_modulation(table, u, v)
                sigma = component.absorption[None, :] * (
                    1.0 + scene.render.particle_modulation_scale * modulation[:, None]
                )
                layer_trans = np.exp(-sigma * optical_segment[:, None])
                layer_alpha = np.clip(1.0 - np.mean(layer_trans, axis=1), 0.0, 1.0)
                visibility = water_shadow_factors(
                    scene, component, self.projection_geometry, entry_point
                )
                light_total, light_colors, _ = _light_term(scene, entry_point, None)
                light_rgb = np.zeros((count, 3), dtype=np.float64)
                for light_index, light in enumerate(scene.lights):
                    light_rgb += (
                        light_colors[light_index][None, :]
                        * visibility[light.light_id][:, None]
                    )
                scatter = component.scattering_color[None, :] * (
                    scene.render.ambient * 0.8
                    + light_rgb * light_total[:, None] * 0.62
                )
                scatter *= (0.62 + 0.55 * modulation[:, None])
                scatter *= layer_alpha[:, None]
                layer_emit = np.minimum(scatter, layer_alpha[:, None])
            trans[:, component_index, :] = np.where(valid[:, None], layer_trans, 1.0)
            emit[:, component_index, :] = np.where(valid[:, None], layer_emit, 0.0)
            alpha[:, component_index] = np.where(valid, layer_alpha, 0.0)

        order = np.argsort(entries, axis=1, kind="stable")
        color = np.zeros((count, 3), dtype=np.float64)
        alpha_out = np.zeros(count, dtype=np.float64)
        transmittance = np.ones((count, 3), dtype=np.float64)
        layer_stats: dict[str, dict[str, float]] = {
            component.component_id: {"transmission": 0.0, "scattering": 0.0}
            for component in scene.components
        }
        for rank in range(component_count):
            selected = order[:, rank]
            active = np.isfinite(entries[pixel_indices, selected]) & (
                entries[pixel_indices, selected] <= solid_t
            )
            layer_trans = trans[pixel_indices, selected]
            layer_emit = emit[pixel_indices, selected]
            layer_alpha = alpha[pixel_indices, selected]
            trans_energy = np.mean(transmittance * layer_trans, axis=1)
            scatter_energy = np.mean(transmittance * layer_emit, axis=1)
            for component_index, component in enumerate(self.transparent_components):
                mask = active & (selected == component_index)
                layer_stats[component.component_id]["transmission"] += float(np.sum(trans_energy[mask]))
                layer_stats[component.component_id]["scattering"] += float(np.sum(scatter_energy[mask]))
            color[active] += (transmittance * layer_emit)[active]
            alpha_out[active] += (np.mean(transmittance, axis=1) * layer_alpha)[active]
            transmittance[active] *= layer_trans[active]

        solid_color = np.zeros((count, 3), dtype=np.float64)
        solid_alpha = np.zeros(count, dtype=np.float64)

        receiver_alpha = np.isfinite(receiver_t)
        receiver_visible = receiver_alpha & (receiver_t <= nearest_fish_t)
        if np.any(receiver_visible):
            ring_mask, factors = receiver_shadow_factors(
                scene, self.projection_geometry, receiver_points
            )
            variation = np.asarray(
                [scene.receiver.pebble_palette[index] for index in _stable_pebble_index(
                    receiver_points, len(scene.receiver.pebble_palette)
                )],
                dtype=np.float64,
            )
            pebble = scene.receiver.base_color[None, :] * 0.45 + variation * 0.55
            light_rgb = np.zeros((count, 3), dtype=np.float64)
            for light in scene.lights:
                delta = light.position[None, :] - receiver_points
                distance2 = np.sum(delta * delta, axis=1)
                light_rgb += (
                    light.color[None, :]
                    * factors[light.light_id][:, None]
                    * (float(light.intensity) / (1.0 + 0.42 * distance2))[:, None]
                )
            floor_radiance = pebble * (scene.render.ambient + 0.42 * light_rgb)
            floor_radiance = np.clip(floor_radiance, 0.0, 1.0)
            solid_color[receiver_visible] = floor_radiance[receiver_visible]
            solid_alpha[receiver_visible] = ring_mask[receiver_visible]

        fish_opaque = np.isfinite(nearest_fish_t) & (nearest_fish_t <= receiver_t)
        if np.any(fish_opaque):
            for index, fish in enumerate(scene.fish):
                mask = fish_opaque & (nearest_fish_index == index)
                if not np.any(mask):
                    continue
                points = fish_points[mask, index, :]
                normal = _normalize(np.mean(fish.normals, axis=0))
                light_total, _, _ = _light_term(scene, points, np.broadcast_to(normal, points.shape))
                shaded = np.clip(
                    fish.color[None, :] * (0.18 + 0.62 * light_total[:, None]),
                    0.0,
                    1.0,
                )
                solid_color[mask] = shaded
                solid_alpha[mask] = fish.opacity

        solid_mask = np.isfinite(solid_t)
        color[solid_mask] += (transmittance * solid_color * solid_alpha[:, None])[solid_mask]
        alpha_out[solid_mask] += (np.mean(transmittance, axis=1) * solid_alpha)[solid_mask]
        transmittance[solid_mask] *= (1.0 - solid_alpha[solid_mask])[:, None]

        # Transparent environment contribution, still premultiplied.
        background_alpha = np.full(count, 0.36, dtype=np.float64)
        color += transmittance * scene.render.background[None, :] * background_alpha[:, None]
        alpha_out += np.mean(transmittance, axis=1) * background_alpha

        color *= scene.render.exposure
        color = np.maximum(color, 0.0)
        alpha_out = np.clip(alpha_out, 0.0, 1.0)
        toned_color = color / (1.0 + color)
        toned_alpha = alpha_out / (1.0 + alpha_out)
        toned_color = np.minimum(toned_color, toned_alpha[:, None])
        unpremultiplied = toned_color / np.maximum(toned_alpha[:, None], 1e-8)
        display_color = np.power(
            np.clip(unpremultiplied, 0.0, 1.0), 1.0 / 2.2
        ) * toned_alpha[:, None]
        rgba = np.empty((count, 4), dtype=np.uint8)
        rgba[:, :3] = np.clip(np.rint(display_color * 255.0), 0, 255).astype(np.uint8)
        rgba[:, 3] = np.clip(np.rint(toned_alpha * 255.0), 0, 255).astype(np.uint8)
        rgba[:, :3] = np.minimum(rgba[:, :3], rgba[:, 3:4])

        surface_t_values: list[np.ndarray] = []
        surface_ids: list[np.ndarray] = []
        for component in self.transparent_components:
            info = intervals[component.component_id]
            surface_t_values.append(np.where(info["valid"], info["entry"], np.inf))
            surface_ids.append(np.full(count, component.ownership_id, dtype=np.int64))
        for index, fish in enumerate(scene.fish):
            surface_t_values.append(fish_t[:, index])
            surface_ids.append(np.full(count, fish.ownership_id, dtype=np.int64))
        surface_t_values.append(receiver_t)
        surface_ids.append(np.full(count, scene.receiver.ownership_id, dtype=np.int64))
        surface_t = np.stack(surface_t_values, axis=1)
        surface_id = np.stack(surface_ids, axis=1)
        owner_index = np.argmin(surface_t, axis=1)
        ownership = surface_id[pixel_indices, owner_index]
        ownership[~np.isfinite(surface_t[pixel_indices, owner_index])] = 0

        return BlockResult(
            rgba=rgba.reshape(height, width, 4),
            ownership=ownership.astype(np.uint16).reshape(height, width),
            layer_stats=layer_stats,
        )

