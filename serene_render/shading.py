from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .scene import Layer, Scene
from .texture import sample_weave


LUMA = np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)


@dataclass(frozen=True)
class SilkShade:
    premultiplied: np.ndarray
    alpha: np.ndarray
    effective_transmittance: np.ndarray
    transmitted_luminance: np.ndarray
    reflected_luminance: np.ndarray


def unit_vectors(vectors: np.ndarray) -> np.ndarray:
    arr = np.asarray(vectors, dtype=np.float64)
    lengths = np.linalg.norm(arr, axis=-1, keepdims=True)
    return arr / np.maximum(lengths, 1.0e-15)


def effective_transmittance(
    scene: Scene,
    layer: Layer,
    texture_transmission: np.ndarray,
    sun_cosine: np.ndarray,
    view_cosine: np.ndarray,
) -> np.ndarray:
    base = float(layer.material["transmittance"])
    path_factor = 0.5 / np.maximum(sun_cosine, 0.16) + 0.5 / np.maximum(
        view_cosine, 0.16
    )
    attenuation = np.exp(
        -scene.thickness_absorption * float(layer.thickness) * path_factor
    )
    return np.clip(base * texture_transmission * attenuation, 0.0, 0.995)


def shade_silk(
    scene: Scene,
    layer: Layer,
    texture,
    world: np.ndarray,
    normal: np.ndarray,
    view_direction: np.ndarray,
    shadow: np.ndarray,
) -> SilkShade:
    points = np.asarray(world, dtype=np.float64).reshape(-1, 3)
    normals = np.asarray(normal, dtype=np.float64).reshape(-1, 3)
    view = np.asarray(view_direction, dtype=np.float64).reshape(-1, 3)
    shadow_factor = np.asarray(shadow, dtype=np.float64).reshape(-1)

    texture_transmission, texture_color = sample_weave(texture, layer, points)
    sun_cosine = np.abs(normals @ scene.sun.direction)
    view_cosine = np.abs(np.sum(normals * view, axis=1))
    coverage = effective_transmittance(
        scene, layer, texture_transmission, sun_cosine, view_cosine
    )

    tint = np.asarray(layer.material["tint"], dtype=np.float64)
    tinted_texture = texture_color * tint[None, :]
    incidence = np.power(np.maximum(sun_cosine, 0.055), 0.72)
    direct = (
        scene.sun.color[None, :]
        * (scene.sun.intensity * incidence * coverage * (1.0 - shadow_factor))[:, None]
        * tinted_texture
    )

    scatter_strength = float(layer.material["scatter_strength"])
    scatter_light = scene.ambient[None, :] + (
        0.23 * scene.sun.intensity * np.maximum(sun_cosine, 0.04)
    )[:, None] * scene.sun.color[None, :]
    scatter = (
        ((1.0 - coverage) * scatter_strength)[:, None]
        * tinted_texture
        * scatter_light
        * (0.78 + 0.22 * texture_transmission)[:, None]
    )

    base_transmission = float(layer.material["transmittance"])
    f0 = ((1.0 - base_transmission) / (1.0 + base_transmission)) ** 2
    fresnel = f0 + (1.0 - f0) * np.power(1.0 - np.clip(view_cosine, 0.0, 1.0), 5.0)
    reflection = (
        fresnel[:, None]
        * incidence[:, None]
        * scene.sun.color[None, :]
        * (0.22 * scene.sun.intensity)
        * (0.35 + 0.65 * tinted_texture)
    )
    reflection += (
        scene.ambient[None, :] * tinted_texture * 0.30
    )

    premultiplied = direct + scatter + reflection
    alpha = np.clip(1.0 - coverage * 0.91, 0.035, 0.995)
    transmitted_luminance = direct @ LUMA
    reflected_luminance = (scatter + reflection) @ LUMA
    return SilkShade(
        premultiplied=premultiplied,
        alpha=alpha,
        effective_transmittance=coverage,
        transmitted_luminance=transmitted_luminance,
        reflected_luminance=reflected_luminance,
    )


def shade_mullion(
    scene: Scene,
    layer: Layer,
    normal: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    normals = np.asarray(normal, dtype=np.float64).reshape(-1, 3)
    color = np.asarray(layer.material["color"], dtype=np.float64)
    reflectance = float(layer.material["reflectance"])
    sun_cosine = np.abs(normals @ scene.sun.direction)
    light = scene.ambient[None, :] * 0.55 + (
        scene.sun.color[None, :]
        * (scene.sun.intensity * (0.35 + 0.65 * sun_cosine))[:, None]
    )
    reflected = color[None, :] * light * (0.35 + 0.65 * reflectance)
    fresnel = reflectance + (1.0 - reflectance) * np.power(
        1.0 - np.clip(sun_cosine, 0.0, 1.0), 5.0
    )
    reflected += (
        fresnel[:, None]
        * scene.sun.color[None, :]
        * (0.10 * scene.sun.intensity)
    )
    alpha = np.ones(reflected.shape[0], dtype=np.float64)
    return reflected, alpha


def ground_shadow_and_color(
    scene: Scene,
    ground_points: np.ndarray,
    shadow: np.ndarray,
) -> np.ndarray:
    points = np.asarray(ground_points, dtype=np.float64).reshape(-1, 3)
    shadow_factor = np.asarray(shadow, dtype=np.float64).reshape(-1)
    sun_cosine = max(0.0, -float(scene.sun.direction[1]))
    albedo = scene.ground.albedo[None, :]
    ambient = scene.ambient[None, :] * 0.70
    direct = (
        scene.sun.color[None, :]
        * (scene.sun.intensity * sun_cosine * (1.0 - shadow_factor))[:, None]
    )
    return albedo * (ambient + direct)


def tone_map_premultiplied(
    premultiplied: np.ndarray,
    alpha: np.ndarray,
    exposure: float,
) -> np.ndarray:
    color = np.asarray(premultiplied, dtype=np.float64)
    alpha_value = np.clip(np.asarray(alpha, dtype=np.float64), 0.0, 1.0)
    safe_alpha = np.maximum(alpha_value[..., None], 1.0e-12)
    unpremultiplied = np.maximum(color, 0.0) / safe_alpha
    mapped = 1.0 - np.exp(-unpremultiplied * exposure)
    srgb = np.power(mapped, 1.0 / 2.2)
    display_premultiplied = srgb * alpha_value[..., None]
    display_rgb = np.floor(
        np.clip(display_premultiplied, 0.0, 1.0) * 255.0 + 0.5
    ).astype(np.uint8)
    display_alpha = np.floor(
        np.clip(alpha_value, 0.0, 1.0) * 255.0 + 0.5
    ).astype(np.uint8)
    return np.concatenate((display_rgb, display_alpha[..., None]), axis=2)
