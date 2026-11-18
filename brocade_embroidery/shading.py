"""Deterministic per-fragment shading for satin and Kesi layers."""

from __future__ import annotations

import math

import numpy as np

from .geometry import EPS, normalize
from .sampling import EmbroideryTable, HoleTable, bilinear_sample
from .shadow import ReceiverField

AMBIENT = np.float32(0.30)
BACKSIDE_AMBIENT = np.float32(0.22)
SPECULAR_CEIL = np.float32(1.35)


def thread_tangent(normal: np.ndarray, thread_direction_deg: float,
                   angle_modulation: np.ndarray) -> np.ndarray:
    """Build a tangent lying perpendicular to each (two-sided) normal."""
    reference = np.array([0.0, 1.0, 0.0])
    if np.abs(np.dot(normal.mean(axis=0), reference)) > 0.92:
        reference = np.array([1.0, 0.0, 0.0])
    base = reference - np.sum(reference * normal, axis=-1, keepdims=True) * normal
    base = normalize(base)
    bitangent = normalize(np.cross(normal, base))
    angle = (math.radians(thread_direction_deg)
             + angle_modulation[..., None] * math.pi)
    tangent = (np.cos(angle) * base + np.sin(angle) * bitangent)
    return normalize(tangent)


def two_sided_normal(normal: np.ndarray, view_vector: np.ndarray) -> np.ndarray:
    facing = np.sum(normal * view_vector, axis=-1, keepdims=True)
    return np.where(facing < 0.0, -normal, normal)


def _ward_anisotropic(
    light_vector: np.ndarray,
    view_vector: np.ndarray,
    normal: np.ndarray,
    tangent: np.ndarray,
    roughness: np.ndarray,
    anisotropy: np.ndarray,
) -> np.ndarray:
    """Clamped anisotropic Ward highlight stretched along the thread.

    Roughness floors and a hard specular ceiling avoid harsh eye-burning
    bands at grazing angles while retaining directional silk sheen.
    """
    half_vector = normalize(light_vector + view_vector)
    bitangent = normalize(np.cross(normal, tangent))
    n_dot_h = np.clip(np.sum(normal * half_vector, axis=-1), EPS, 1.0)
    t_dot_h = np.sum(tangent * half_vector, axis=-1)
    b_dot_h = np.sum(bitangent * half_vector, axis=-1)
    alpha_x = np.clip(roughness * (1.45 - 0.65 * anisotropy), 0.08, 0.95)
    alpha_y = np.clip(roughness * (0.55 + 0.75 * anisotropy), 0.08, 0.95)
    exponent = -2.0 * (
        (t_dot_h / alpha_x) ** 2 + (b_dot_h / alpha_y) ** 2
    ) / (1.0 + n_dot_h)
    denominator = np.maximum(
        4.0 * math.pi * alpha_x * alpha_y
        * np.sqrt(
            np.clip(np.sum(normal * light_vector, axis=-1), EPS, 1.0)
            * np.clip(np.sum(normal * view_vector, axis=-1), EPS, 1.0)
        ),
        EPS,
    )
    specular = np.exp(np.clip(exponent, -40.0, 0.0)) / denominator
    return np.minimum(specular.astype(np.float32), SPECULAR_CEIL)


def _wrap_diffuse(n_dot_l: np.ndarray) -> np.ndarray:
    # Wrap lighting keeps back-facing satin readable instead of collapsing
    # to black; the transition itself is smooth and deterministic.
    return np.clip((n_dot_l + 0.35) / 1.35, 0.0, 1.0).astype(np.float32)


def _shadow_factor(layer_index: int | str, world_points: np.ndarray,
                   light_id: str,
                   fields: dict[tuple, ReceiverField]) -> np.ndarray:
    key = (layer_index, light_id)
    field = fields.get(key)
    if field is None:
        return np.ones(world_points.shape[0], dtype=np.float32)
    delta = world_points - field.origin
    u = np.dot(delta, field.u_axis)
    v = np.dot(delta, field.v_axis)
    u = (u + 0.5 * field.extent_u) / field.extent_u
    v = (v + 0.5 * field.extent_v) / field.extent_v
    inside = ((u >= 0.0) & (u <= 1.0) & (v >= 0.0) & (v <= 1.0))
    values = bilinear_sample(field.transmission, u, v)
    return np.where(inside, values, 1.0).astype(np.float32)


def inter_layer_attenuation(air_gap: float, view_vector: np.ndarray,
                            normal: np.ndarray) -> np.ndarray:
    """Deterministic gap/incidence attenuation for light crossing layers.

    A thicker declared air gap and a more grazing incidence attenuate the
    accumulated back-light slightly more.  The formula is continuous, seeded
    only by scene constants and per-fragment geometry, so it cannot produce
    order-dependent flicker.
    """
    cosine = np.clip(np.abs(np.sum(normal * view_vector, axis=-1)),
                     1e-3, 1.0).astype(np.float32)
    grazing = np.float32(1.0) - cosine
    gap = np.float32(0.04 * max(0.0, float(air_gap)))
    return np.clip(np.float32(1.0) - gap * (np.float32(0.35)
                                            + np.float32(0.65) * grazing),
                   0.72, 1.0).astype(np.float32)


def shade_satin(
    layer: dict,
    world_points: np.ndarray,
    normals: np.ndarray,
    uv: np.ndarray,
    camera_eye: np.ndarray,
    lights: list[dict],
    table: EmbroideryTable,
    shadow_fields: dict[tuple, ReceiverField],
) -> tuple[np.ndarray, np.ndarray]:
    """Return linear RGB and a scalar reflection contribution weight."""
    material = layer["material"]
    base_color = np.asarray(material["base_color"], dtype=np.float32)
    roughness = np.float32(material["roughness"])
    reflectivity = np.float32(material["anisotropic_reflectivity"])

    view_vector = normalize(camera_eye[None, :] - world_points)
    normal = two_sided_normal(normalize(normals), view_vector)
    modulation = bilinear_sample(table.modulation, uv[:, 0], uv[:, 1])
    angle_mod = bilinear_sample(table.thread_angle, uv[:, 0], uv[:, 1])
    anisotropy = bilinear_sample(table.anisotropy_scale, uv[:, 0], uv[:, 1])
    tangent = thread_tangent(normal, material["thread_direction_deg"],
                             angle_mod)

    albedo = base_color[None, :] * (
        np.float32(0.62) + np.float32(0.55) * modulation[:, None])
    color = albedo * AMBIENT
    back_facing = np.sum(normalize(normals) * view_vector, axis=-1) < 0.0
    color += albedo * (BACKSIDE_AMBIENT * back_facing.astype(np.float32))[:, None]
    reflection_weight = np.zeros(world_points.shape[0], dtype=np.float32)

    for light in lights:
        light_position = np.asarray(light["position"], dtype=np.float64)
        light_color = np.asarray(light["color"], dtype=np.float32)
        light_vector = normalize(light_position[None, :] - world_points)
        distance = np.linalg.norm(
            light_position[None, :] - world_points, axis=-1)
        attenuation = np.float32(light["intensity"]) / (
            np.float32(1.0) + np.float32(0.018)
            * (distance * distance).astype(np.float32))
        n_dot_l = np.sum(normal * light_vector, axis=-1)
        diffuse = _wrap_diffuse(n_dot_l)
        shadow = _shadow_factor(layer["layer_index"], world_points,
                                light["id"], shadow_fields)
        specular = _ward_anisotropic(
            light_vector, view_vector, normal, tangent,
            roughness * np.ones(world_points.shape[0], dtype=np.float32),
            anisotropy,
        )
        light_contribution = (
            albedo * diffuse[:, None]
            + (reflectivity * specular)[:, None]
        )
        color += (light_contribution * attenuation[:, None]
                  * light_color[None, :] * shadow[:, None])
        reflection_weight += attenuation * reflectivity * specular * shadow
    return np.clip(color, 0.0, 1.5).astype(np.float32), reflection_weight


def shade_kesi(
    layer: dict,
    world_points: np.ndarray,
    normals: np.ndarray,
    uv: np.ndarray,
    camera_eye: np.ndarray,
    lights: list[dict],
    table: HoleTable,
    shadow_fields: dict[tuple, ReceiverField],
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Return RGB, premultiplied alpha and a transmission contribution."""
    material = layer["material"]
    base_color = np.asarray(material["base_color"], dtype=np.float32)
    transmittance = np.float32(material["transmittance"])
    coverage = bilinear_sample(table.coverage, uv[:, 0], uv[:, 1])
    view_vector = normalize(camera_eye[None, :] - world_points)
    normal = two_sided_normal(normalize(normals), view_vector)
    albedo = base_color[None, :] * (
        np.float32(0.75) + np.float32(0.35) * coverage[:, None])
    color = albedo * AMBIENT
    transmission_weight = np.zeros(world_points.shape[0], dtype=np.float32)

    for light in lights:
        light_position = np.asarray(light["position"], dtype=np.float64)
        light_color = np.asarray(light["color"], dtype=np.float32)
        light_vector = normalize(light_position[None, :] - world_points)
        distance = np.linalg.norm(
            light_position[None, :] - world_points, axis=-1)
        attenuation = np.float32(light["intensity"]) / (
            np.float32(1.0) + np.float32(0.018)
            * (distance * distance).astype(np.float32))
        n_dot_l = np.sum(normal * light_vector, axis=-1)
        diffuse = _wrap_diffuse(n_dot_l)
        shadow = _shadow_factor(layer["layer_index"], world_points,
                                light["id"], shadow_fields)
        # Weak near-isotropic sheen for Kesi silk; holes cannot sparkle.
        half_vector = normalize(light_vector + view_vector)
        n_dot_h = np.clip(np.sum(normal * half_vector, axis=-1), EPS, 1.0)
        sheen = np.float32(0.18) * np.power(n_dot_h, 18.0).astype(np.float32)
        solid_light = (albedo * diffuse[:, None]
                       + sheen[:, None]) * attenuation[:, None]
        color += solid_light * light_color[None, :] * shadow[:, None]
        transmitted = (
            attenuation * transmittance * (np.float32(1.0) - coverage) * shadow
        )
        color += (transmitted[:, None] * light_color[None, :]
                  * np.float32(0.9))
        transmission_weight += transmitted
    alpha = np.clip(
        np.float32(0.92) * (np.float32(0.45) + np.float32(0.55) * coverage),
        0.0, 1.0,
    ).astype(np.float32)
    color = np.clip(color, 0.0, 1.5).astype(np.float32)
    return color, alpha, transmission_weight
