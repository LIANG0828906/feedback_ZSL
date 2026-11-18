from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .camera import Camera
from .raster import LayerHits
from .tables import brocade_pattern, material_tables, sample_tables
from .validation import LayerData, SceneData


@dataclass
class ShadedLayer:
    rgb_straight: np.ndarray
    premultiplied: np.ndarray
    alpha: np.ndarray
    visible_fragments: int
    transmission_energy: float
    reflection_energy: float
    effective_transmittance_sum: float


def tone_map_rgb(
    radiance: np.ndarray,
    exposure: float,
    white_cap: float,
) -> np.ndarray:
    mapped = 1.0 - np.exp(-np.maximum(radiance, 0.0) * exposure)
    srgb = np.where(
        mapped <= 0.0031308,
        12.92 * mapped,
        1.055 * np.power(mapped, 1.0 / 2.4) - 0.055,
    )
    return np.clip(srgb, 0.0, white_cap)


def shade_layer(
    scene: SceneData,
    layer: LayerData,
    hits: LayerHits,
    camera: Camera,
) -> ShadedLayer:
    height, width = hits.mask.shape
    straight = np.zeros((height, width, 3), dtype=np.float64)
    alpha_image = np.zeros((height, width), dtype=np.float64)
    visible = int(np.count_nonzero(hits.mask))
    transmission_energy = 0.0
    reflection_energy = 0.0
    effective_sum = 0.0
    if visible == 0:
        return ShadedLayer(
            rgb_straight=straight,
            premultiplied=np.zeros((height, width, 4), dtype=np.float64),
            alpha=alpha_image,
            visible_fragments=0,
            transmission_energy=0.0,
            reflection_energy=0.0,
            effective_transmittance_sum=0.0,
        )

    points = hits.point[hits.mask]
    uv = hits.uv[hits.mask]
    geometric_normal = hits.normal[hits.mask]
    view = camera.position[None, :] - points
    view_length = np.linalg.norm(view, axis=1)
    view_unit = view / np.maximum(view_length[:, None], 1e-12)
    facing = np.einsum("nc,nc->n", geometric_normal, view_unit)
    normal = np.where(facing[:, None] < 0.0, -geometric_normal, geometric_normal)

    tables = material_tables(layer.pattern_seed)
    table_pattern, normal_u, normal_v = sample_tables(tables, uv[:, 0], uv[:, 1])
    pattern = brocade_pattern(table_pattern, uv[:, 0], uv[:, 1], layer.pattern_seed)

    reference = np.where(
        np.abs(normal[:, 1:2]) < 0.9,
        np.array([0.0, 1.0, 0.0])[None, :],
        np.array([1.0, 0.0, 0.0])[None, :],
    )
    tangent = np.cross(reference, normal)
    tangent_length = np.linalg.norm(tangent, axis=1)
    tangent /= np.maximum(tangent_length[:, None], 1e-12)
    bitangent = np.cross(normal, tangent)
    perturbed = normal + scene.shading["fold_normal_strength"] * (
        normal_u[:, None] * tangent + normal_v[:, None] * bitangent
    )
    perturbed_length = np.linalg.norm(perturbed, axis=1)
    perturbed /= np.maximum(perturbed_length[:, None], 1e-12)
    perturbed = np.where(
        np.einsum("nc,nc->n", perturbed, normal)[:, None] < 0.0,
        -perturbed,
        perturbed,
    )

    incident_cosine = np.abs(np.einsum("nc,nc->n", normal, view_unit))
    thickness_ratio = (
        layer.thickness
        / scene.shading["transmission_reference_thickness"]
    )
    optical_path_ratio = (
        thickness_ratio
        / np.maximum(incident_cosine, scene.shading["min_cosine"])
    )
    effective_transmittance = np.power(
        np.maximum(layer.transmittance, 1e-12),
        optical_path_ratio,
    )
    if layer.transmittance <= 0.0:
        effective_transmittance[:] = 0.0

    alpha = np.clip(
        1.0
        - scene.shading["alpha_transmission_weight"] * effective_transmittance,
        scene.shading["alpha_floor"],
        0.98,
    )
    fold_crease = normal_u - 0.65 * normal_v
    fold_factor = np.clip(
        1.0
        + 0.34
        * scene.shading["fold_normal_strength"]
        * fold_crease,
        0.62,
        1.25,
    )
    ambient = (
        layer.scatter_color[None, :]
        * scene.shading["ambient_strength"]
        * (0.58 + 0.42 * pattern)[:, None]
        * fold_factor[:, None]
    )
    transmission = np.zeros_like(ambient)
    reflection = np.zeros_like(ambient)

    for light in scene.lights:
        delta = light.position[None, :] - points
        distance_squared = np.einsum("nc,nc->n", delta, delta)
        light_unit = delta / np.maximum(
            np.sqrt(distance_squared)[:, None], 1e-12
        )
        alignment = np.einsum("nc,nc->n", perturbed, light_unit)
        back_facing = np.clip(-alignment, 0.0, 1.0)
        front_facing = np.clip(alignment, 0.0, 1.0)
        attenuation = light.intensity / (0.55 + distance_squared)
        transmit_weight = (
            attenuation
            * effective_transmittance
            * (0.42 + 0.58 * pattern)
            * (0.28 + 0.72 * back_facing)
            * fold_factor
        )
        reflect_weight = (
            attenuation
            * (0.34 + 0.66 * pattern)
            * front_facing
            * fold_factor
        )
        transmission += (
            layer.scatter_color[None, :]
            * light.color[None, :]
            * transmit_weight[:, None]
        )
        reflection += (
            layer.scatter_color[None, :]
            * light.color[None, :]
            * reflect_weight[:, None]
        )

    radiance = transmission + reflection + ambient
    display = tone_map_rgb(
        radiance,
        scene.tone_map_exposure,
        scene.tone_map_white_cap,
    )
    straight[hits.mask] = display
    alpha_image[hits.mask] = alpha
    premultiplied = np.zeros((height, width, 4), dtype=np.float64)
    premultiplied[hits.mask, :3] = display * alpha[:, None]
    premultiplied[hits.mask, 3] = alpha

    energy_weights = alpha
    transmission_energy = float(
        np.sum(np.sum(transmission, axis=1) * energy_weights)
    )
    reflection_energy = float(
        np.sum(np.sum(reflection + ambient, axis=1) * energy_weights)
    )
    effective_sum = float(np.sum(effective_transmittance))
    return ShadedLayer(
        rgb_straight=straight,
        premultiplied=premultiplied,
        alpha=alpha_image,
        visible_fragments=visible,
        transmission_energy=transmission_energy,
        reflection_energy=reflection_energy,
        effective_transmittance_sum=effective_sum,
    )

