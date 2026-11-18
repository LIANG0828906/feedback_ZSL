"""Camera rays, analytic louver intersections, materials and layer compositing."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .patterns import PatternSet
from .projection import ProjectionSystem
from .scene import Leaf, Light, Receiver, Scene
from .util import smoothstep, stable_u64

RECEIVER_SPOT_GAIN = 0.085


@dataclass
class LayerHit:
    leaf_index: np.ndarray
    t: np.ndarray
    u: np.ndarray
    v: np.ndarray
    point: np.ndarray
    normal: np.ndarray
    backside: np.ndarray
    hit: np.ndarray


@dataclass
class PixelShade:
    rgba: np.ndarray
    owner_id: np.ndarray
    owner_layer: np.ndarray
    reflection_sums: np.ndarray
    transmission_sums: np.ndarray
    hit_counts: np.ndarray
    max_specular: float
    clipped_before_tonemap: int
    receiver_pixels: int


def camera_rays(
    scene: Scene, angle_index: int, pixel_x: np.ndarray, pixel_y: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    camera = scene.camera
    angle_rad = math.radians(camera.angles_deg[angle_index])
    elevation_rad = math.radians(camera.elevation_deg)
    position = camera.center + np.asarray(
        [
            camera.radius * math.cos(elevation_rad) * math.sin(angle_rad),
            camera.radius * math.sin(elevation_rad),
            camera.radius * math.cos(elevation_rad) * math.cos(angle_rad),
        ],
        dtype=np.float64,
    )
    forward = camera.center - position
    forward /= np.linalg.norm(forward)
    world_up = np.asarray([0.0, 1.0, 0.0], dtype=np.float64)
    right = np.cross(forward, world_up)
    right /= np.linalg.norm(right)
    up = np.cross(right, forward)
    aspect = camera.width / camera.height
    tan_half = math.tan(math.radians(camera.fov_deg) * 0.5)
    px = np.asarray(pixel_x, dtype=np.float64)
    py = np.asarray(pixel_y, dtype=np.float64)
    ndc_x = (2.0 * (px + 0.5) / camera.width - 1.0) * tan_half * aspect
    ndc_y = (1.0 - 2.0 * (py + 0.5) / camera.height) * tan_half
    directions = (
        forward[None, :]
        + ndc_x[:, None] * right[None, :]
        + ndc_y[:, None] * up[None, :]
    )
    directions /= np.maximum(
        np.linalg.norm(directions, axis=1, keepdims=True), 1e-15
    )
    origins = np.broadcast_to(position, directions.shape).copy()
    return origins, directions


def intersect_layer(
    origins: np.ndarray, directions: np.ndarray, leaves: list[Leaf]
) -> LayerHit:
    count = origins.shape[0]
    best_t = np.full(count, np.inf, dtype=np.float64)
    best_leaf = np.full(count, -1, dtype=np.int64)
    best_u = np.zeros(count, dtype=np.float64)
    best_v = np.zeros(count, dtype=np.float64)
    best_face = np.zeros(count, dtype=np.int8)
    for leaf_index, leaf in enumerate(leaves):
        dz = directions[:, 2]
        valid_z = np.abs(dz) > 1e-12
        safe_dz = np.where(valid_z, dz, 1.0)
        t_front = (leaf.z_front - origins[:, 2]) / safe_dz
        t_back = (leaf.z_back - origins[:, 2]) / safe_dz
        take_front = t_front <= t_back
        t_candidate = np.where(take_front, t_front, t_back)
        face = np.where(take_front, 1, -1).astype(np.int8)
        x = origins[:, 0] + t_candidate * directions[:, 0]
        y = origins[:, 1] + t_candidate * directions[:, 1]
        inside = (
            valid_z
            & np.isfinite(t_candidate)
            & (t_candidate > 1e-6)
            & (x >= leaf.rect_x[0])
            & (x <= leaf.rect_x[1])
            & (y >= leaf.rect_y[0])
            & (y <= leaf.rect_y[1])
        )
        update = inside & (t_candidate < best_t)
        if np.any(update):
            best_t[update] = t_candidate[update]
            best_leaf[update] = leaf_index
            best_u[update] = np.clip(
                (x[update] - leaf.rect_x[0]) / leaf.width, 0.0, 1.0
            )
            best_v[update] = np.clip(
                (y[update] - leaf.rect_y[0]) / leaf.height, 0.0, 1.0
            )
            best_face[update] = face[update]
    hit = best_leaf >= 0
    safe_t = np.where(hit, best_t, 0.0)
    point = origins + safe_t[:, None] * directions
    normal = np.zeros_like(point)
    normal[:, 2] = best_face
    rotated = directions @ np.asarray([0.0, 0.0, 1.0])
    backside = hit & (rotated > 0.0)
    normal[backside, 2] = -1.0
    return LayerHit(
        leaf_index=best_leaf,
        t=safe_t,
        u=best_u,
        v=best_v,
        point=point,
        normal=normal,
        backside=backside,
        hit=hit,
    )


def intersect_receivers(
    scene: Scene, origins: np.ndarray, directions: np.ndarray
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    count = origins.shape[0]
    best_t = np.full(count, np.inf, dtype=np.float64)
    receiver_code = np.full(count, -1, dtype=np.int8)
    for code, receiver_id in enumerate(("wall", "floor")):
        receiver = scene.receivers[receiver_id]
        axis_index = {"x": 0, "y": 1, "z": 2}[receiver.plane_axis]
        denominator = directions[:, axis_index]
        valid = np.abs(denominator) > 1e-12
        safe = np.where(valid, denominator, 1.0)
        t = (receiver.plane_value - origins[:, axis_index]) / safe
        point = origins + t[:, None] * directions
        inside = valid & np.isfinite(t) & (t > 1e-6)
        for axis, bound in receiver.bounds.items():
            index = {"x": 0, "y": 1, "z": 2}[axis]
            inside &= (point[:, index] >= bound[0]) & (point[:, index] <= bound[1])
        update = inside & (t < best_t)
        receiver_code[update] = code
        best_t[update] = t[update]
    hit = receiver_code >= 0
    safe_t = np.where(hit, best_t, 0.0)
    point = origins + safe_t[:, None] * directions
    return receiver_code, safe_t, point


def _value_noise(
    axis_a: np.ndarray, axis_b: np.ndarray, scale: float, seed: int
) -> np.ndarray:
    ga = np.floor(axis_a * scale).astype(np.int64)
    gb = np.floor(axis_b * scale).astype(np.int64)
    fa = axis_a * scale - ga
    fb = axis_b * scale - gb
    fa = fa * fa * (3.0 - 2.0 * fa)
    fb = fb * fb * (3.0 - 2.0 * fb)
    from .util import hash_unit_array

    v00 = hash_unit_array(ga, gb, seed)
    v10 = hash_unit_array(ga + 1, gb, seed)
    v01 = hash_unit_array(ga, gb + 1, seed)
    v11 = hash_unit_array(ga + 1, gb + 1, seed)
    top = v00 * (1.0 - fa) + v10 * fa
    bottom = v01 * (1.0 - fa) + v11 * fa
    return top * (1.0 - fb) + bottom * fb


def receiver_surface_color(scene: Scene, points: np.ndarray, code: np.ndarray) -> np.ndarray:
    from .util import hash_unit_array

    color = np.zeros_like(points)
    wall_mask = code == 0
    floor_mask = code == 1
    if np.any(wall_mask):
        wall = scene.receivers["wall"]
        p = points[wall_mask]
        broad = _value_noise(p[:, 0], p[:, 1], 4.3, stable_u64(8011, 1))
        grain = _value_noise(p[:, 0], p[:, 1], 31.0, stable_u64(8011, 2))
        shade = 0.91 + 0.10 * broad + 0.035 * (grain - 0.5)
        color[wall_mask] = wall.albedo[None, :] * shade[:, None]
    if np.any(floor_mask):
        floor = scene.receivers["floor"]
        p = points[floor_mask]
        broad = _value_noise(p[:, 0], p[:, 2], 3.2, stable_u64(8023, 1))
        mid = _value_noise(p[:, 0], p[:, 2], 12.0, stable_u64(8023, 2))
        cell_x = np.floor(p[:, 0] * 56.0).astype(np.int64)
        cell_z = np.floor(p[:, 2] * 56.0).astype(np.int64)
        chip_hash = hash_unit_array(cell_x, cell_z, stable_u64(8023, 3))
        chip = (chip_hash > 0.87).astype(np.float64)
        fleck_tone = (chip_hash - 0.87) / 0.13
        base = floor.albedo[None, :] * (
            0.82 + 0.24 * broad + 0.08 * (mid - 0.5)
        )[:, None]
        warm_chip = np.asarray([0.105, 0.090, 0.072], dtype=np.float64)
        cool_chip = np.asarray([-0.030, -0.025, -0.018], dtype=np.float64)
        chip_color = np.where(
            (cell_x % 3)[:, None] == 0,
            warm_chip[None, :],
            cool_chip[None, :],
        ) * fleck_tone[:, None]
        color[floor_mask] = np.maximum(
            base + chip[:, None] * chip_color, 0.015
        )
    return color


def _light_terms(
    light: Light, points: np.ndarray, normals: np.ndarray
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    vector = light.position[None, :] - points
    distance = np.linalg.norm(vector, axis=1)
    direction = vector / np.maximum(distance[:, None], 1e-12)
    cosine_axis = (-direction) @ light.axis
    spot = smoothstep(light.cos_outer, light.cos_inner, cosine_axis)
    falloff = light.intensity / np.power(
        1.0 + distance * distance, float(light.falloff) * 0.5
    )
    normal_dot_light = np.sum(direction * normals, axis=1)
    beam = np.maximum(falloff * spot, 0.0)
    return beam, direction, normal_dot_light, distance


def _anisotropic_ggx(
    normals: np.ndarray,
    view: np.ndarray,
    direction: np.ndarray,
    tangent: np.ndarray,
    bitangent: np.ndarray,
    roughness_t: np.ndarray,
    roughness_b: np.ndarray,
    specular: float,
) -> np.ndarray:
    half_vector = view + direction
    half_vector /= np.maximum(
        np.linalg.norm(half_vector, axis=1, keepdims=True), 1e-12
    )
    n_dot_h = np.maximum(np.sum(normals * half_vector, axis=1), 1e-5)
    h_t = np.sum(half_vector * tangent, axis=1) / np.maximum(roughness_t, 1e-4)
    h_b = np.sum(half_vector * bitangent, axis=1) / np.maximum(roughness_b, 1e-4)
    denominator = n_dot_h * n_dot_h + h_t * h_t + h_b * h_b
    distribution = 1.0 / (
        np.pi
        * np.maximum(roughness_t, 1e-4)
        * np.maximum(roughness_b, 1e-4)
        * np.maximum(denominator * denominator, 1e-12)
    )
    n_dot_v = np.maximum(np.sum(normals * view, axis=1), 1e-5)
    n_dot_l = np.maximum(np.sum(normals * direction, axis=1), 1e-5)
    v_dot_h = np.maximum(np.sum(view * half_vector, axis=1), 0.0)
    fresnel = specular + (1.0 - specular) * np.power(1.0 - v_dot_h, 5.0)
    k_t = roughness_t * 0.5
    k_b = roughness_b * 0.5
    g_v_t = n_dot_v / (n_dot_v * (1.0 - k_t) + k_t + 1e-12)
    g_l_t = n_dot_l / (n_dot_l * (1.0 - k_t) + k_t + 1e-12)
    g_v_b = n_dot_v / (n_dot_v * (1.0 - k_b) + k_b + 1e-12)
    g_l_b = n_dot_l / (n_dot_l * (1.0 - k_b) + k_b + 1e-12)
    geometry = np.sqrt(g_v_t * g_l_t * g_v_b * g_l_b)
    return np.maximum(
        distribution
        * geometry
        * fresnel
        / np.maximum(4.0 * n_dot_v * n_dot_l, 1e-8),
        0.0,
    )


def shade_leaves(
    scene: Scene,
    patterns: PatternSet,
    angle_index: int,
    origins: np.ndarray,
    directions: np.ndarray,
    hits: list[LayerHit],
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray, float]:
    count = origins.shape[0]
    layer_premult = np.zeros((len(scene.layers), count, 3), dtype=np.float64)
    layer_alpha = np.zeros((len(scene.layers), count), dtype=np.float64)
    reflection_sums = np.zeros(len(scene.layers), dtype=np.float64)
    transmission_sums = np.zeros(len(scene.layers), dtype=np.float64)
    hit_counts = np.zeros(len(scene.layers), dtype=np.int64)
    specular_map = np.zeros(count, dtype=np.float64)
    max_specular = 0.0
    view = -directions
    ordered_layers = sorted(scene.layers, key=lambda item: item.layer)
    for out_layer_index, layer in enumerate(ordered_layers):
        hit = hits[out_layer_index]
        if not np.any(hit.hit):
            continue
        mask = hit.hit
        indices = hit.leaf_index[mask]
        layer_leaves = sorted(
            scene.leaves_by_layer[layer.layer], key=lambda item: item.leaf
        )
        u = hit.u[mask]
        v = hit.v[mask]
        points = hit.point[mask]
        normals = hit.normal[mask]
        view_local = view[mask]
        aperture = np.empty(indices.shape[0], dtype=np.float64)
        micro = np.empty((indices.shape[0], 2), dtype=np.float64)
        albedo = np.empty((indices.shape[0], 3), dtype=np.float64)
        rough_t = np.empty(indices.shape[0], dtype=np.float64)
        rough_b = np.empty(indices.shape[0], dtype=np.float64)
        specular = np.empty(indices.shape[0], dtype=np.float64)
        thickness = np.empty(indices.shape[0], dtype=np.float64)
        brush = np.empty((indices.shape[0], 3), dtype=np.float64)
        for local_index, leaf in enumerate(layer_leaves):
            selection = np.flatnonzero(indices == local_index)
            if selection.size == 0:
                continue
            aperture[selection] = patterns.sample_aperture(
                leaf.layer, leaf.leaf, u[selection], v[selection]
            )
            micro[selection] = patterns.sample_microsurface(
                leaf.layer, leaf.leaf, u[selection], v[selection]
            )
            albedo[selection] = leaf.albedo
            rough_t[selection] = leaf.roughness_along
            rough_b[selection] = leaf.roughness_across
            specular[selection] = leaf.specular
            thickness[selection] = leaf.thickness
            brush[selection] = leaf.brush_direction
        surface_factor = np.clip(1.0 - aperture * 0.92, 0.045, 1.0)
        tangent = brush - normals * np.sum(brush * normals, axis=1, keepdims=True)
        tangent_length = np.linalg.norm(tangent, axis=1, keepdims=True)
        fallback = np.asarray([1.0, 0.0, 0.0], dtype=np.float64)
        tangent = np.where(tangent_length > 1e-9, tangent, fallback[None, :])
        tangent /= np.maximum(np.linalg.norm(tangent, axis=1, keepdims=True), 1e-12)
        bitangent = np.cross(normals, tangent)
        perturbed = normals + tangent * (micro[:, 0:1] * 0.105) + bitangent * (
            micro[:, 1:2] * 0.085
        )
        perturbed /= np.maximum(
            np.linalg.norm(perturbed, axis=1, keepdims=True), 1e-12
        )
        roughness_t = np.clip(
            rough_t * (1.0 + 0.26 * micro[:, 0] + 0.08 * np.abs(micro[:, 1])),
            0.04,
            0.75,
        )
        roughness_b = np.clip(
            rough_b * (1.0 + 0.12 * micro[:, 1]), 0.08, 0.95
        )
        ambient = scene.render.ambient_strength
        reflection = albedo * ambient * (0.34 + 0.66 * surface_factor[:, None])
        transmission = 0.045 * aperture[:, None] * albedo
        spec_accum = np.zeros(indices.shape[0], dtype=np.float64)
        for light in sorted(scene.lights, key=lambda item: item.id.encode("utf-8")):
            beam, light_dir, normal_dot_light, _ = _light_terms(
                light, points, perturbed
            )
            front = np.maximum(normal_dot_light, 0.0)
            back = np.maximum(-normal_dot_light, 0.0)
            diffuse = (
                albedo
                * light.color[None, :]
                * (beam * front * 0.018 * surface_factor)[:, None]
            )
            spec = _anisotropic_ggx(
                perturbed,
                view_local,
                light_dir,
                tangent,
                bitangent,
                roughness_t,
                roughness_b,
                specular,
            )
            knee = max(scene.render.highlight_soft_knee, 1e-4)
            spec = spec / (1.0 + knee * spec)
            spec *= (0.78 + 0.22 * (0.5 + 0.5 * micro[:, 0])) * surface_factor
            spec *= front * beam * 0.098
            spec = np.minimum(spec, 1.30)
            max_specular = max(max_specular, float(np.max(spec)) if spec.size else 0.0)
            spec_color = light.color[None, :] * spec[:, None]
            reflection += diffuse + spec_color
            spec_accum += spec
            transmit_gain = (
                beam
                * aperture
                * (back * 0.22 + np.maximum(normal_dot_light, 0.0) * 0.050)
            )
            transmission += (
                light.color[None, :]
                * albedo
                * transmit_gain[:, None]
            )
        incidence = np.clip(
            np.abs(np.sum(perturbed * view_local, axis=1)), 0.0, 1.0
        )
        coupling = 0.72 + 0.28 * np.exp(
            -scene.render.interlayer_gap / (thickness + 0.04)
        ) * incidence
        reflection *= coupling[:, None]
        transmission *= coupling[:, None]
        alpha = np.clip(0.055 + 0.925 * (1.0 - aperture), 0.0, 0.985)
        radiance = reflection + transmission
        premult = radiance * alpha[:, None]
        layer_alpha[out_layer_index, mask] = alpha
        layer_premult[out_layer_index, mask, :] = premult
        reflection_sums[out_layer_index] = float(
            np.sum(np.mean(reflection, axis=1) * alpha)
        )
        transmission_sums[out_layer_index] = float(
            np.sum(np.mean(transmission, axis=1) * alpha)
        )
        hit_counts[out_layer_index] = int(np.count_nonzero(mask))
        specular_map[mask] = spec_accum
    return (
        layer_premult,
        layer_alpha,
        reflection_sums,
        transmission_sums,
        hit_counts,
        specular_map,
        max_specular,
    )


def shade_pixels(
    scene: Scene,
    patterns: PatternSet,
    projection: ProjectionSystem,
    angle_index: int,
    origins: np.ndarray,
    directions: np.ndarray,
) -> PixelShade:
    count = origins.shape[0]
    leaves_by_layer = scene.leaves_by_layer
    hits = [
        intersect_layer(origins, directions, list(leaves_by_layer[layer.layer]))
        for layer in sorted(scene.layers, key=lambda item: item.layer)
    ]
    receiver_code, receiver_t, receiver_point = intersect_receivers(
        scene, origins, directions
    )
    receiver_hit = receiver_code >= 0
    rgba = np.zeros((count, 4), dtype=np.float64)
    receiver_pixels = int(np.count_nonzero(receiver_hit))
    if receiver_pixels:
        wall_mask = receiver_code == 0
        floor_mask = receiver_code == 1
        surface = receiver_surface_color(scene, receiver_point, receiver_code)
        base = surface * scene.render.ambient_strength
        for mask, receiver_id, normal_value in (
            (wall_mask, "wall", np.asarray([0.0, 0.0, 1.0])),
            (floor_mask, "floor", np.asarray([0.0, 1.0, 0.0])),
        ):
            if not np.any(mask):
                continue
            points = receiver_point[mask]
            normals = np.broadcast_to(normal_value, points.shape).copy()
            spot_rgb, _ = projection.receiver_radiance(points, normals)
            base[mask] += surface[mask] * spot_rgb * RECEIVER_SPOT_GAIN
        rgba[receiver_hit, 0:3] = np.maximum(base[receiver_hit], 0.0)
        rgba[receiver_hit, 3] = 1.0

    (
        layer_premult,
        layer_alpha,
        reflection_sums,
        transmission_sums,
        hit_counts,
        spec_map,
        max_specular,
    ) = shade_leaves(scene, patterns, angle_index, origins, directions, hits)
    for layer_index in range(len(scene.layers)):
        alpha = layer_alpha[layer_index]
        layer_mask = alpha > 0.0
        if not np.any(layer_mask):
            continue
        rgba[layer_mask, 0:3] = (
            layer_premult[layer_index, layer_mask, :]
            + (1.0 - alpha[layer_mask, None]) * rgba[layer_mask, 0:3]
        )
        rgba[layer_mask, 3] = (
            alpha[layer_mask] + (1.0 - alpha[layer_mask]) * rgba[layer_mask, 3]
        )

    remaining = np.ones(count, dtype=np.float64)
    best_strength = np.zeros(count, dtype=np.float64)
    owner_id = np.full(count, -1, dtype=np.int32)
    owner_layer = np.full(count, 255, dtype=np.uint8)
    owner_id[receiver_code == 0] = -2
    owner_id[receiver_code == 1] = -3
    for layer_index in range(len(scene.layers) - 1, -1, -1):
        alpha = layer_alpha[layer_index]
        strength = alpha * remaining
        update = strength > best_strength + 1e-12
        if np.any(update):
            hit = hits[layer_index]
            local_indices = hit.leaf_index
            layer_leaves = sorted(
                scene.leaves_by_layer[scene.layers[layer_index].layer],
                key=lambda item: item.leaf,
            )
            leaf_ids = np.full(count, -1, dtype=np.int32)
            valid_leaf = local_indices >= 0
            leaf_ids[valid_leaf] = np.asarray(
                [leaf.leaf for leaf in layer_leaves], dtype=np.int32
            )[local_indices[valid_leaf]]
            selected = update & (strength >= 0.015) & valid_leaf
            owner_id[selected] = (
                scene.layers[layer_index].layer * 10000 + leaf_ids[selected] + 1
            )
            owner_layer[selected] = scene.layers[layer_index].layer
            best_strength[update] = strength[update]
        remaining *= 1.0 - alpha

    clipped = int(np.count_nonzero(rgba[:, 0:3] > 1.0))
    return PixelShade(
        rgba=rgba.astype(np.float32),
        owner_id=owner_id,
        owner_layer=owner_layer,
        reflection_sums=reflection_sums,
        transmission_sums=transmission_sums,
        hit_counts=hit_counts,
        max_specular=max_specular,
        clipped_before_tonemap=clipped,
        receiver_pixels=receiver_pixels,
    )
