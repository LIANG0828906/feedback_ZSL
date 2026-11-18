"""Deterministic layered transfer renderer."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .geometry import (
    LayerGeometry,
    build_layer_geometries,
    camera_rays,
    lattice_pattern,
    raycast_layer,
    sample_texture,
)
from .projection import visibility_for_points
from .texture import generate_weave_texture

_LUMA = np.asarray([0.2126, 0.7152, 0.0722], dtype=np.float64)


def _luminance(rgb: np.ndarray) -> np.ndarray:
    return np.asarray(rgb, dtype=np.float64) @ _LUMA


def _compress_rgb(rgb: np.ndarray) -> np.ndarray:
    """Hue-preserving highlight compression."""

    rgb = np.asarray(rgb, dtype=np.float64)
    luminance = np.maximum(_luminance(rgb), 0.0)
    mapped = 1.0 - np.exp(-luminance)
    scale = np.divide(
        mapped,
        luminance,
        out=np.zeros_like(mapped),
        where=luminance > 1e-12,
    )
    return rgb * scale[..., None]


@dataclass
class RenderContext:
    scene: dict[str, Any]
    geometries: list[LayerGeometry]
    textures: dict[int, np.ndarray]

    @classmethod
    def create(cls, scene: dict[str, Any]) -> "RenderContext":
        geometries = build_layer_geometries(scene)
        textures: dict[int, np.ndarray] = {}
        for geometry in geometries:
            if geometry.kind == "gauze":
                texture = generate_weave_texture(
                    int(geometry.definition["weave_seed"]),
                    geometry.definition["resolution"],
                )
                geometry.texture = texture
                textures[geometry.ordinal] = texture
        return cls(scene=scene, geometries=geometries, textures=textures)


def _light_energy(
    points: np.ndarray,
    normals: np.ndarray,
    visibility: np.ndarray,
    scene: dict[str, Any],
    *,
    ground: bool,
) -> np.ndarray:
    result = np.zeros((points.shape[0], 3), dtype=np.float64)
    falloff_setting = float(scene["global"]["light_falloff"])
    for light_index, light in enumerate(scene["lights"]):
        light_position = np.asarray(light["position"], dtype=np.float64)
        delta = light_position[None, :] - points
        distance_squared = np.sum(delta * delta, axis=1)
        direction = delta / np.sqrt(np.maximum(distance_squared, 1e-18))[:, None]
        dot = np.sum(normals * direction, axis=1)
        if ground:
            angle = np.clip(dot, 0.0, 1.0)
        else:
            angle = 0.28 + 0.72 * np.abs(dot)
        attenuation = 1.0 / (1.0 + falloff_setting * distance_squared)
        strength = (
            float(light["intensity"])
            * attenuation
            * angle
            * visibility[:, light_index]
        )
        color = np.asarray(light["color"], dtype=np.float64)
        result += strength[:, None] * color[None, :]
    return result


def _ground_color(
    points: np.ndarray,
    scene: dict[str, Any],
    context: RenderContext,
) -> np.ndarray:
    normals = np.broadcast_to(
        np.asarray([0.0, 1.0, 0.0]), points.shape
    ).copy()
    visibility = visibility_for_points(
        points,
        None,
        scene["lights"],
        context.geometries,
        scene,
    )
    energy = _light_energy(points, normals, visibility, scene, ground=True)
    compressed = _compress_rgb(energy)
    color = np.asarray(scene["ground"]["color"], dtype=np.float64)
    ambient = float(scene["global"]["ambient"])
    result = color[None, :] * (
        ambient + float(scene["ground"]["reflectance"]) * 0.92
    ) + color[None, :] * compressed * 2.2
    return np.clip(result, 0.0, 1.0)


def _gauze_transfer(
    geometry: LayerGeometry,
    points: np.ndarray,
    normals: np.ndarray,
    view_directions: np.ndarray,
    visibility: np.ndarray,
    scene: dict[str, Any],
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    layer = geometry.definition
    texture = geometry.texture
    assert texture is not None
    uv = geometry.uv(points)
    weave = sample_texture(texture, uv, float(layer["texture_repeat"]))
    cosine = np.clip(
        np.abs(np.sum(normals * (-view_directions), axis=1)),
        0.22,
        1.0,
    )
    base_transmittance = float(layer["transmittance"]) * (0.76 + 0.44 * weave)
    base_transmittance = np.clip(base_transmittance, 0.002, 0.995)
    thickness_path = (float(layer["thickness"]) / 0.006) / cosine
    effective_transmittance = base_transmittance ** thickness_path
    tint_mix = np.clip(0.54 + 0.40 * weave, 0.0, 1.0)
    tint = np.asarray(layer["tint"], dtype=np.float64)
    trans_color = (
        (1.0 - tint_mix[:, None]) * np.ones_like(normals)
        + tint_mix[:, None] * tint[None, :]
    )
    trans_color *= effective_transmittance[:, None]
    t_alpha = np.max(trans_color, axis=1)

    energy = _light_energy(points, normals, visibility, scene, ground=False)
    energy_luminance = _luminance(_compress_rgb(energy))
    tint_texture = np.clip(tint[None, :] * (0.84 + 0.28 * weave[:, None]), 0.0, 1.0)
    scatter_strength = float(scene["global"]["scatter_strength"])
    scatter = (
        tint_texture
        * energy_luminance[:, None]
        * scatter_strength
        * (0.34 + 0.82 * weave[:, None])
        * (0.62 + 0.38 * cosine[:, None])
    )
    scatter += (
        tint_texture
        * float(scene["global"]["ambient"])
        * (0.16 + 0.20 * weave[:, None])
    )
    scatter = np.clip(scatter, 0.0, 1.0)
    transmittance_metric = t_alpha.copy()
    return trans_color, scatter, t_alpha, transmittance_metric


def _lattice_transfer(
    geometry: LayerGeometry,
    points: np.ndarray,
    normals: np.ndarray,
    visibility: np.ndarray,
    scene: dict[str, Any],
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    layer = geometry.definition
    coverage, _distance = lattice_pattern(geometry, points)
    coverage = np.clip(coverage, 0.0, 1.0)
    trans_color = np.repeat((1.0 - coverage)[:, None], 3, axis=1)
    t_alpha = 1.0 - coverage
    energy = _light_energy(points, normals, visibility, scene, ground=False)
    energy_luminance = _luminance(_compress_rgb(energy))
    metal_color = np.asarray(layer["metal_color"], dtype=np.float64)
    reflectance = float(layer["reflectance"])
    reflection = (
        metal_color[None, :]
        * reflectance
        * energy_luminance[:, None]
        * (0.42 + 0.58 * coverage[:, None])
    )
    reflection += (
        metal_color[None, :]
        * reflectance
        * float(scene["global"]["ambient"])
        * 0.18
    )
    reflection = np.clip(reflection, 0.0, 1.0)
    return trans_color, reflection, coverage, t_alpha


def render_tile(
    scene: dict[str, Any],
    context: RenderContext,
    x0: int,
    y0: int,
    tile_width: int,
    tile_height: int,
) -> tuple[np.ndarray, np.ndarray, dict[str, np.ndarray]]:
    image = scene["image"]
    origins, directions, sample_count = camera_rays(
        scene["camera"],
        int(image["width"]),
        int(image["height"]),
        x0,
        y0,
        tile_width,
        tile_height,
    )
    flat_origins = origins.reshape(-1, 3)
    flat_directions = directions.reshape(-1, 3)
    flat_count = flat_origins.shape[0]

    background = np.asarray(image["background_rgba"], dtype=np.float64)
    premultiplied = np.broadcast_to(
        background[:3] * background[3], (flat_count, 3)
    ).copy()
    alpha = np.full(flat_count, background[3], dtype=np.float64)

    ground = scene["ground"]
    ground_hit = np.zeros(flat_count, dtype=bool)
    ground_points = np.full((flat_count, 3), np.nan, dtype=np.float64)
    vertical_direction = flat_directions[:, 1]
    valid_vertical = np.abs(vertical_direction) > 1e-10
    ground_t = np.full(flat_count, np.inf, dtype=np.float64)
    ground_t[valid_vertical] = (
        float(ground["y"]) - float(scene["camera"]["position"][1])
    ) / vertical_direction[valid_vertical]
    ground_hit = (
        valid_vertical
        & (ground_t >= float(scene["camera"]["near"]))
        & (ground_t <= float(scene["camera"]["far"]))
    )
    if np.any(ground_hit):
        points = flat_origins[ground_hit] + flat_directions[ground_hit] * ground_t[
            ground_hit, None
        ]
        inside_bounds = (
            (points[:, 0] >= float(ground["x"][0]))
            & (points[:, 0] <= float(ground["x"][1]))
            & (points[:, 2] >= float(ground["z"][0]))
            & (points[:, 2] <= float(ground["z"][1]))
        )
        hit_indices = np.nonzero(ground_hit)[0]
        ground_hit[:] = False
        ground_hit[hit_indices[inside_bounds]] = True
        ground_points[ground_hit] = points[inside_bounds]
        if np.any(ground_hit):
            ground_colors = _ground_color(ground_points[ground_hit], scene, context)
            premultiplied[ground_hit] = ground_colors
            alpha[ground_hit] = 1.0

    layer_count = len(context.geometries)
    hit_counts = np.zeros(layer_count, dtype=np.uint64)
    transmittance_sums = np.zeros(layer_count, dtype=np.float64)
    transmission_sums = np.zeros(layer_count, dtype=np.float64)
    reflection_sums = np.zeros(layer_count, dtype=np.float64)
    owner_weight = np.zeros((layer_count, flat_count), dtype=np.float64)
    covered = np.zeros((layer_count, flat_count), dtype=bool)
    remaining = np.ones(flat_count, dtype=np.float64)

    camera_near = float(scene["camera"]["near"])
    camera_far = float(scene["camera"]["far"])
    for geometry in context.geometries:
        intersection = raycast_layer(
            flat_origins,
            flat_directions,
            geometry,
            camera_near,
            camera_far,
        )
        hit = intersection["hit"]
        if not np.any(hit):
            continue
        hit_indices = np.nonzero(hit)[0]
        points = intersection["point"][hit]
        normals = intersection["normal"][hit]
        view_directions = flat_directions[hit]
        visibility = visibility_for_points(
            points,
            geometry.layer,
            scene["lights"],
            context.geometries,
            scene,
        )
        if geometry.kind == "gauze":
            trans_color, scatter, t_alpha, transmittance_metric = _gauze_transfer(
                geometry,
                points,
                normals,
                view_directions,
                visibility,
                scene,
            )
        else:
            trans_color, scatter, t_alpha, transmittance_metric = _lattice_transfer(
                geometry,
                points,
                normals,
                visibility,
                scene,
            )
            t_alpha = np.clip(t_alpha, 0.0, 1.0)

        layer_alpha = np.clip(1.0 - t_alpha, 0.0, 1.0)
        owner_weight[geometry.ordinal, hit_indices] += (
            remaining[hit_indices] * layer_alpha
        )
        covered[geometry.ordinal, hit_indices] = True

        incoming = np.divide(
            premultiplied[hit_indices],
            np.maximum(alpha[hit_indices, None], 1e-12),
        )
        transmission_sums[geometry.ordinal] += float(
            np.sum(_luminance(incoming * trans_color))
        )
        reflection_sums[geometry.ordinal] += float(
            np.sum(_luminance(scatter * layer_alpha[:, None]))
        )
        hit_counts[geometry.ordinal] += int(hit_indices.shape[0])
        transmittance_sums[geometry.ordinal] += float(np.sum(transmittance_metric))

        premultiplied[hit_indices] = (
            premultiplied[hit_indices] * trans_color + scatter * layer_alpha[:, None]
        )
        alpha[hit_indices] = (
            alpha[hit_indices] * t_alpha + layer_alpha
        )
        remaining[hit_indices] *= t_alpha

    premultiplied = np.clip(premultiplied, 0.0, 1.0)
    alpha = np.clip(alpha, 0.0, 1.0)
    premultiplied = np.minimum(premultiplied, alpha[:, None])

    premultiplied_image = premultiplied.reshape(
        sample_count, tile_height, tile_width, 3
    ).mean(axis=0)
    alpha_image = alpha.reshape(sample_count, tile_height, tile_width).mean(axis=0)
    premultiplied_image = np.minimum(premultiplied_image, alpha_image[..., None])
    rgba_float = np.concatenate((premultiplied_image, alpha_image[..., None]), axis=2)
    rgba = np.rint(np.clip(rgba_float, 0.0, 1.0) * 255.0).astype(np.uint8)

    weights_image = owner_weight.reshape(
        layer_count, sample_count, tile_height, tile_width
    ).sum(axis=1)
    covered_image = covered.reshape(
        layer_count, sample_count, tile_height, tile_width
    ).any(axis=1)
    visible_count = np.sum(covered_image, axis=0, dtype=np.int64)
    dominant = np.zeros((tile_height, tile_width), dtype=np.int64)
    if layer_count:
        dominant = np.argmax(weights_image, axis=0).astype(np.int64) + 1
    dominant[visible_count == 0] = 0
    ownership = np.zeros((tile_height, tile_width, 4), dtype=np.uint8)
    ownership[..., 0] = ((dominant >> 8) & 0xFF).astype(np.uint8)
    ownership[..., 1] = (dominant & 0xFF).astype(np.uint8)
    ownership[..., 2] = np.clip(visible_count, 0, 255).astype(np.uint8)
    ownership[..., 3] = np.where(visible_count > 0, 255, 0).astype(np.uint8)

    stats = {
        "hit_count": hit_counts,
        "transmittance_sum": transmittance_sums,
        "transmission_contribution_sum": transmission_sums,
        "reflection_contribution_sum": reflection_sums,
    }
    return rgba, ownership, stats


def ownership_layer_manifest(scene: dict[str, Any]) -> dict[str, Any]:
    return {
        "schema_version": "veil-ownership/1",
        "encoding": {
            "R,G": "16-bit big-endian dominant layer ordinal, zero means no layer",
            "B": "number of distinct layers with at least one hit at the pixel",
            "A": "255 iff at least one layer was hit",
        },
        "layers": [
            {
                "ordinal": index + 1,
                "layer": int(layer["layer"]),
                "id": str(layer["id"]),
                "kind": str(layer["kind"]),
            }
            for index, layer in enumerate(scene["layers"])
        ],
    }

