from __future__ import annotations

import math
from typing import Any

import numpy as np

from .io_utils import sha256_bytes, stable_float
from .procedural import LayerTexture, TextureSet
from .scene import CameraSpec, LayerSpec, LightSpec, SceneSpec, ShadowSpec, WallSpec


def projection_offsets(density: int) -> np.ndarray:
    if density <= 1:
        return np.zeros((1, 2), dtype=np.float64)
    values = np.linspace(-1.0, 1.0, density, dtype=np.float64)
    x_grid, y_grid = np.meshgrid(values, values)
    keep = x_grid * x_grid + y_grid * y_grid <= 1.0 + 1e-12
    offsets = np.column_stack([x_grid[keep], y_grid[keep]])
    center = np.zeros((1, 2), dtype=np.float64)
    return np.vstack([center, offsets])


def layer_transmission(
    source: LayerSpec,
    texture: LayerTexture,
    receiver_points: np.ndarray,
    light: LightSpec,
    shadow: ShadowSpec,
    offsets: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    points = np.asarray(receiver_points, dtype=np.float64).reshape(-1, 3)
    source_denominator = source.plane_z - float(light.position[2])
    if source_denominator <= 1e-12:
        raise ValueError("light must lie before the innermost layer along +Z")
    scale = (points[:, 2] - float(light.position[2])) / source_denominator
    scale = np.maximum(scale, 1e-12)
    source_points = light.position[None, :] + (points - light.position[None, :]) / scale[:, None]
    uv = source.world_to_uv(source_points)
    gap = np.maximum(points[:, 2] - source.plane_z, 0.0)
    radius_receiver = shadow.penumbra_per_gap * gap
    radius_source = radius_receiver / scale
    world_width = max(float(source.xy_max[0] - source.xy_min[0]), 1e-12)
    world_height = max(float(source.xy_max[1] - source.xy_min[1]), 1e-12)
    radius_uv = np.column_stack(
        [
            radius_source / world_width,
            radius_source / world_height,
        ]
    )

    base_tau = source.liner.transmittance
    accumulated = np.zeros(points.shape[0], dtype=np.float64)
    for offset in offsets:
        sample_uv = uv + radius_uv * offset[None, :]
        inside = (
            (sample_uv[:, 0] >= 0.0)
            & (sample_uv[:, 0] <= 1.0)
            & (sample_uv[:, 1] >= 0.0)
            & (sample_uv[:, 1] <= 1.0)
        )
        values = np.ones(points.shape[0], dtype=np.float64)
        if np.any(inside):
            fiber, hole = _sample_texture(texture, sample_uv[inside])
            fiber_modulation = 0.82 + 0.36 * fiber
            tau = hole + (1.0 - hole) * np.clip(
                base_tau * fiber_modulation,
                0.0,
                1.0,
            )
            values[inside] = tau
        accumulated += values
    return accumulated / max(1, offsets.shape[0]), uv


def accumulated_transmission(
    previous_layers: list[LayerSpec],
    textures: TextureSet,
    receiver_points: np.ndarray,
    light: LightSpec,
    shadow: ShadowSpec,
    offsets: np.ndarray,
) -> np.ndarray:
    points = np.asarray(receiver_points, dtype=np.float64).reshape(-1, 3)
    result = np.ones(points.shape[0], dtype=np.float64)
    for source in previous_layers:
        transmission, _ = layer_transmission(
            source,
            textures.layers[source.layer],
            points,
            light,
            shadow,
            offsets,
        )
        result *= transmission
    return result


def project_source_uv(
    source: LayerSpec,
    source_uv: tuple[float, float],
    target_z: float,
    light: LightSpec,
) -> np.ndarray:
    source_world = source.uv_to_world(np.asarray(source_uv, dtype=np.float64))
    denominator = source.plane_z - float(light.position[2])
    if denominator <= 1e-12:
        raise ValueError("light must lie before the innermost layer along +Z")
    scale = (target_z - float(light.position[2])) / denominator
    return light.position + scale * (source_world - light.position)


def project_source_uv_to_wall(
    source: LayerSpec,
    source_uv: tuple[float, float],
    wall: WallSpec,
    light: LightSpec,
) -> np.ndarray:
    source_world = source.uv_to_world(np.asarray(source_uv, dtype=np.float64))
    direction = source_world - light.position
    denominator = float(np.dot(direction, wall.normal))
    if abs(denominator) <= 1e-12:
        raise ValueError("projection ray is parallel to the wall")
    t = float(np.dot(wall.point - light.position, wall.normal) / denominator)
    return light.position + t * direction


def choose_hole_uv(texture: LayerTexture, preferred: tuple[float, float] = (0.46, 0.53)) -> tuple[float, float]:
    ys, xs = np.nonzero(texture.hole_u8 >= 128)
    if xs.size == 0:
        raise ValueError(f"layer {texture.layer} has no open hole sample")
    u = (xs.astype(np.float64) + 0.5) / texture.width
    v = (ys.astype(np.float64) + 0.5) / texture.height
    score = (u - preferred[0]) ** 2 + (v - preferred[1]) ** 2
    index = int(np.argmin(score))
    return float(u[index]), float(v[index])


def build_projection_report(
    scene: SceneSpec,
    textures: TextureSet,
    camera: CameraSpec,
    grayscale_maps: dict[str, np.ndarray] | None = None,
) -> tuple[dict[str, Any], dict[str, np.ndarray]]:
    offsets = projection_offsets(scene.shadow.sampling_density)
    pairs: list[dict[str, Any]] = []
    for index, source in enumerate(scene.layers):
        source_uv = choose_hole_uv(textures.layers[source.layer])
        source_world = source.uv_to_world(np.asarray(source_uv))
        if index + 1 < len(scene.layers):
            target = scene.layers[index + 1]
            target_point = project_source_uv(
                source,
                source_uv,
                target.plane_z,
                scene.light,
            )
            target_name = f"layer_{target.layer}"
            target_uv = target.world_to_uv(target_point.reshape(1, 3))[0]
            within = bool(0.0 <= target_uv[0] <= 1.0 and 0.0 <= target_uv[1] <= 1.0)
            gap = max(0.0, target.plane_z - source.plane_z)
            receiver_points = target_point.reshape(1, 3)
            transmission, _ = layer_transmission(
                source,
                textures.layers[source.layer],
                receiver_points,
                scene.light,
                scene.shadow,
                offsets,
            )
            openness = float(transmission[0])
            edge_width = 2.0 * scene.shadow.penumbra_per_gap * gap
            profile = _projection_profile(
                scene,
                textures,
                source,
                destination_point=target_point,
                destination_z=target.plane_z,
                axis=np.array([1.0, 0.0, 0.0]),
                extent=max(0.08, 4.0 * edge_width),
                offsets=offsets,
                layers=[source],
            )
            pair_id = f"{source.layer}->{target.layer}"
            pairs.append(
                {
                    "pair": pair_id,
                    "source_layer": source.layer,
                    "target": target_name,
                    "target_layer": target.layer,
                    "source_uv": [stable_float(source_uv[0]), stable_float(source_uv[1])],
                    "source_world": [stable_float(value) for value in source_world],
                    "target_world": [stable_float(value) for value in target_point],
                    "target_uv": [stable_float(target_uv[0]), stable_float(target_uv[1])],
                    "gap": stable_float(gap),
                    "penumbra_radius_world": stable_float(0.5 * edge_width),
                    "edge_width_world": stable_float(edge_width),
                    "edge_width_pixels": stable_float(
                        edge_width / _world_per_pixel(camera, scene, target_point)
                    ),
                    "within_target_geometry": within,
                    "projected_luma": stable_float(openness),
                    "profile_samples": profile,
                }
            )
        wall_point = project_source_uv_to_wall(source, source_uv, scene.wall, scene.light)
        gap = max(0.0, float(wall_point[2] - source.plane_z))
        edge_width = 2.0 * scene.shadow.penumbra_per_gap * gap
        transmission, _ = layer_transmission(
            source,
            textures.layers[source.layer],
            wall_point.reshape(1, 3),
            scene.light,
            scene.shadow,
            offsets,
        )
        profile = _projection_profile(
            scene,
            textures,
            source,
            destination_point=wall_point,
            destination_z=float(wall_point[2]),
            axis=np.array([1.0, 0.0, 0.0]),
            extent=max(0.08, 4.0 * edge_width),
            offsets=offsets,
            layers=[source],
        )
        pair_id = f"{source.layer}->wall"
        pairs.append(
            {
                "pair": pair_id,
                "source_layer": source.layer,
                "target": "inner_wall",
                "target_layer": None,
                "source_uv": [stable_float(source_uv[0]), stable_float(source_uv[1])],
                "source_world": [stable_float(value) for value in source_world],
                "target_world": [stable_float(value) for value in wall_point],
                "target_uv": None,
                "gap": stable_float(gap),
                "penumbra_radius_world": stable_float(0.5 * edge_width),
                "edge_width_world": stable_float(edge_width),
                "edge_width_pixels": stable_float(
                    edge_width / _world_per_pixel(camera, scene, wall_point)
                ),
                "within_target_geometry": True,
                "projected_luma": stable_float(float(transmission[0])),
                "profile_samples": profile,
            }
        )

    if grayscale_maps is None:
        grayscale_maps = {
            pair["pair"]: np.zeros((scene.image.height, scene.image.width), dtype=np.uint8)
            for pair in pairs
        }
    grayscale_maps = dict(grayscale_maps)
    combined = np.zeros((scene.image.height, scene.image.width), dtype=np.uint8)
    for pair in pairs:
        combined = np.maximum(combined, grayscale_maps[pair["pair"]])
    grayscale_maps["combined"] = combined
    report = {
        "schema_version": 1,
        "sampling_density": scene.shadow.sampling_density,
        "sample_count": int(offsets.shape[0]),
        "penumbra_per_gap": stable_float(scene.shadow.penumbra_per_gap),
        "pairs": pairs,
        "grayscale_sha256": {
            pair_id: sha256_bytes(np.ascontiguousarray(image).tobytes())
            for pair_id, image in sorted(grayscale_maps.items())
        },
    }
    return report, grayscale_maps


def _projection_profile(
    scene: SceneSpec,
    textures: TextureSet,
    source: LayerSpec,
    *,
    destination_point: np.ndarray,
    destination_z: float,
    axis: np.ndarray,
    extent: float,
    offsets: np.ndarray,
    layers: list[LayerSpec],
) -> list[dict[str, Any]]:
    steps = 41
    distances = np.linspace(-extent, extent, steps, dtype=np.float64)
    points = destination_point[None, :] + distances[:, None] * axis[None, :]
    points[:, 2] = destination_z
    transmission = np.ones(points.shape[0], dtype=np.float64)
    for layer in layers:
        values, _ = layer_transmission(
            layer,
            textures.layers[layer.layer],
            points,
            scene.light,
            scene.shadow,
            offsets,
        )
        transmission *= values
    edge_width = 2.0 * scene.shadow.penumbra_per_gap * max(0.0, destination_z - source.plane_z)
    sample_distances = np.array(
        [-edge_width, -0.5 * edge_width, 0.0, 0.5 * edge_width, edge_width],
        dtype=np.float64,
    )
    samples: list[dict[str, Any]] = []
    for distance in sample_distances:
        value = float(
            np.interp(
                distance,
                distances,
                transmission,
            )
        )
        samples.append(
            {
                "offset_world": stable_float(distance),
                "luma": stable_float(value),
            }
        )
    return samples


def _world_per_pixel(camera: CameraSpec, scene: SceneSpec, point: np.ndarray) -> float:
    depth = float(np.dot(point - camera.position, camera.forward))
    depth = max(depth, camera.near)
    return 2.0 * depth * math.tan(math.radians(camera.fov_y_degrees) * 0.5) / scene.image.height


def _sample_texture(texture: LayerTexture, uv: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    from .procedural import _sample_bilinear

    fiber = _sample_bilinear(texture.fiber_u8, uv).astype(np.float64) / 255.0
    hole = _sample_bilinear(texture.hole_u8, uv).astype(np.float64) / 255.0
    return fiber, hole
