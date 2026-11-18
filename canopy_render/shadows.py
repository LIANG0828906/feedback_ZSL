from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .camera import Camera, intersect_wall_rays, wall_uv
from .shading import tone_map_rgb
from .util import stable_round
from .validation import LightData, SceneData, TasselData, TasselSegment, WallData

GOLDEN_ANGLE = np.pi * (3.0 - np.sqrt(5.0))


@dataclass
class WallRender:
    rgb: np.ndarray
    alpha: np.ndarray
    rays_evaluated: int


def _f(value: float, digits: int = 7) -> float:
    return stable_round(value, digits)


def _vec3(values: np.ndarray) -> list[float]:
    return [_f(float(v)) for v in values]


def light_disk_samples(
    light: LightData,
    wall: WallData,
    sample_count: int,
) -> np.ndarray:
    to_wall = wall.origin - light.position
    to_wall /= max(float(np.linalg.norm(to_wall)), 1e-12)
    helper = np.array([0.0, 1.0, 0.0])
    if abs(float(np.dot(to_wall, helper))) > 0.94:
        helper = np.array([1.0, 0.0, 0.0])
    basis_u = np.cross(to_wall, helper)
    basis_u /= max(float(np.linalg.norm(basis_u)), 1e-12)
    basis_v = np.cross(to_wall, basis_u)
    basis_v /= max(float(np.linalg.norm(basis_v)), 1e-12)

    samples = np.empty((sample_count, 3), dtype=np.float64)
    for index in range(sample_count):
        radius = np.sqrt((index + 0.5) / sample_count) * light.radius
        angle = index * GOLDEN_ANGLE
        samples[index] = (
            light.position
            + basis_u * (radius * np.cos(angle))
            + basis_v * (radius * np.sin(angle))
        )
    return samples


def visibility_fraction(
    points: np.ndarray,
    light_samples: np.ndarray,
    segments: tuple[TasselSegment, ...],
) -> np.ndarray:
    """Area-light visibility of wall points against capsule tassel segments."""
    point_count = len(points)
    visible_count = np.zeros(point_count, dtype=np.int64)
    sample_count = len(light_samples)
    for sample in light_samples:
        direction = sample[None, :] - points
        ray_length_squared = np.einsum("nc,nc->n", direction, direction)
        occluded = np.zeros(point_count, dtype=bool)
        for segment in segments:
            segment_direction = segment.b - segment.a
            relative = points - segment.a[None, :]
            c = float(np.dot(segment_direction, segment_direction))
            b = direction @ segment_direction
            d = np.einsum("nc,nc->n", direction, relative)
            e = relative @ segment_direction
            denominator = ray_length_squared * c - b * b
            usable = np.abs(denominator) > 1e-14
            safe = np.where(usable, denominator, 1.0)
            ray_t = (b * e - c * d) / safe
            segment_u = (ray_length_squared * e - b * d) / safe
            t_clamped = np.clip(ray_t, 0.0, 1.0)
            u_clamped = np.clip(segment_u, 0.0, 1.0)
            closest = (
                points
                + direction * t_clamped[:, None]
                - (
                    segment.a[None, :]
                    + segment_direction[None, :] * u_clamped[:, None]
                )
            )
            distance_squared = np.einsum("nc,nc->n", closest, closest)
            hit = (
                usable
                & (ray_t > 1e-7)
                & (ray_t < 1.0 - 1e-7)
                & (segment_u >= -1e-7)
                & (segment_u <= 1.0 + 1e-7)
                & (distance_squared <= segment.radius * segment.radius)
            )
            occluded |= hit
        visible_count += (~occluded).astype(np.int64)
    return visible_count.astype(np.float64) / float(sample_count)


def all_segments(scene: SceneData) -> tuple[TasselSegment, ...]:
    return tuple(
        segment
        for tassel in scene.tassels
        for segment in tassel.segments
    )


def render_wall_tile(
    scene: SceneData,
    camera: Camera,
    x0: int,
    y0: int,
    width: int,
    height: int,
) -> WallRender:
    rays = camera.rays(x0, y0, width, height)
    wall = scene.wall
    points, _, hit = intersect_wall_rays(
        camera, wall.origin, wall.normal, rays
    )
    u, v = wall_uv(points, wall.origin, wall.u_axis, wall.v_axis)
    inside = (
        hit
        & (np.abs(u) <= wall.half_u)
        & (np.abs(v) <= wall.half_v)
    )
    output_rgb = np.zeros((height, width, 3), dtype=np.float64)
    output_alpha = np.zeros((height, width), dtype=np.float64)
    if not np.any(inside):
        return WallRender(rgb=output_rgb, alpha=output_alpha, rays_evaluated=0)

    point_list = points[inside]
    u_list = u[inside]
    v_list = v[inside]
    stone = (
        0.93
        + 0.035 * np.sin(np.pi * 2.0 * (u_list * 3.1 + v_list * 1.7))
        + 0.025 * np.sin(np.pi * 2.0 * (u_list * 7.3 - v_list * 4.1 + 0.17))
    )
    radiance = wall.base_color[None, :] * (0.29 * stone[:, None])
    rays_evaluated = 0
    segments = all_segments(scene)
    for light in scene.lights:
        if light.cast_shadow:
            samples = light_disk_samples(
                light,
                wall,
                scene.shadows["samples_per_light"],
            )
            visibility = visibility_fraction(point_list, samples, segments)
            rays_evaluated += int(len(point_list) * len(samples))
        else:
            visibility = np.ones(len(point_list), dtype=np.float64)
        delta = light.position[None, :] - point_list
        distance_squared = np.einsum("nc,nc->n", delta, delta)
        direction = delta / np.maximum(
            np.sqrt(distance_squared)[:, None], 1e-12
        )
        incidence = np.clip(
            np.einsum("nc,c->n", direction, wall.normal), 0.0, 1.0
        )
        attenuation = light.intensity / (1.15 + distance_squared)
        contribution = (
            attenuation
            * incidence
            * visibility
        )[:, None] * (
            wall.base_color[None, :] * light.color[None, :]
        )
        radiance += contribution

    display = tone_map_rgb(
        radiance,
        scene.tone_map_exposure,
        scene.tone_map_white_cap,
    )
    output_rgb[inside] = display
    output_alpha[inside] = 1.0
    return WallRender(
        rgb=output_rgb,
        alpha=output_alpha,
        rays_evaluated=rays_evaluated,
    )


def project_point_from_light(
    light_position: np.ndarray,
    point: np.ndarray,
    wall: WallData,
) -> dict[str, Any]:
    direction = point - light_position
    denominator = float(np.dot(direction, wall.normal))
    if abs(denominator) < 1e-12:
        return {
            "status": "parallel",
            "wall_point": None,
            "wall_uv": None,
            "inside_wall": False,
            "ray_parameter": None,
        }
    parameter = float(np.dot(wall.origin - light_position, wall.normal) / denominator)
    projected = light_position + parameter * direction
    relative = projected - wall.origin
    u = float(np.dot(relative, wall.u_axis))
    v = float(np.dot(relative, wall.v_axis))
    inside = abs(u) <= wall.half_u and abs(v) <= wall.half_v
    if parameter <= 0.0:
        status = "behind_light"
    elif not inside:
        status = "outside_wall"
    else:
        status = "projected"
    return {
        "status": status,
        "wall_point": projected,
        "wall_uv": np.array([u, v]),
        "inside_wall": bool(inside),
        "ray_parameter": parameter,
    }


def projection_catalogue(scene: SceneData) -> list[dict[str, Any]]:
    catalogue: list[dict[str, Any]] = []
    wall = scene.wall
    for tassel in scene.tassels:
        for segment in tassel.segments:
            source = 0.5 * (segment.a + segment.b)
            for light in scene.lights:
                projected = project_point_from_light(
                    light.position,
                    source,
                    wall,
                )
                wall_point = projected["wall_point"]
                wall_uv_value = projected["wall_uv"]
                catalogue.append(
                    {
                        "tassel_id": tassel.tassel_id,
                        "segment_index": segment.segment_index,
                        "light_id": light.light_id,
                        "source_point": _vec3(source),
                        "wall_point": (
                            _vec3(wall_point)
                            if wall_point is not None
                            else None
                        ),
                        "wall_uv": (
                            [_f(float(wall_uv_value[0])), _f(float(wall_uv_value[1]))]
                            if wall_uv_value is not None
                            else None
                        ),
                        "inside_wall": projected["inside_wall"],
                        "status": projected["status"],
                        "ray_parameter": (
                            _f(float(projected["ray_parameter"]))
                            if projected["ray_parameter"] is not None
                            else None
                        ),
                        "distance_to_wall": _f(
                            abs(float(np.dot(source - wall.origin, wall.normal)))
                        ),
                    }
                )
    return catalogue


def _intersect_single_ray_with_wall(
    camera: Camera,
    direction: np.ndarray,
    wall: WallData,
) -> tuple[np.ndarray | None, tuple[float, float] | None]:
    denominator = float(np.dot(direction, wall.normal))
    if abs(denominator) < 1e-12:
        return None, None
    numerator = float(np.dot(wall.origin - camera.position, wall.normal))
    t = numerator / denominator
    if t <= camera.near or t >= camera.far:
        return None, None
    point = camera.position + t * direction
    relative = point - wall.origin
    u = float(np.dot(relative, wall.u_axis))
    v = float(np.dot(relative, wall.v_axis))
    if abs(u) > wall.half_u or abs(v) > wall.half_v:
        return None, None
    return point, (u, v)


def _crossing_distance(
    distances: np.ndarray,
    brightness: np.ndarray,
    level: float,
) -> float | None:
    for index in range(1, len(distances)):
        previous = float(brightness[index - 1])
        current = float(brightness[index])
        if previous == current:
            if current >= level:
                return float(distances[index])
            continue
        if (previous - level) * (current - level) <= 0.0:
            fraction = (level - previous) / (current - previous)
            return float(
                distances[index - 1]
                + fraction * (distances[index] - distances[index - 1])
            )
    return None


def _soft_edge_measurement(
    scene: SceneData,
    camera: Camera,
    tassel: TasselData,
    light: LightData,
) -> dict[str, Any]:
    wall = scene.wall
    segment = tassel.segments[0]
    source = 0.5 * (segment.a + segment.b)
    center_projection = project_point_from_light(light.position, source, wall)
    result: dict[str, Any] = {
        "tassel_id": tassel.tassel_id,
        "light_id": light.light_id,
        "distance_to_wall": _f(
            abs(float(np.dot(source - wall.origin, wall.normal)))
        ),
        "status": center_projection["status"],
        "transition_width_px": 0.0,
        "left_width_px": None,
        "right_width_px": None,
        "shadowed_brightness": None,
        "unshadowed_brightness": None,
        "samples": [],
    }
    if (
        center_projection["status"] != "projected"
        or center_projection["wall_point"] is None
    ):
        return result

    projected_a = project_point_from_light(light.position, segment.a, wall)
    projected_b = project_point_from_light(light.position, segment.b, wall)
    if (
        projected_a["wall_point"] is None
        or projected_b["wall_point"] is None
    ):
        direction_uv = np.array([1.0, 0.0])
    else:
        point_a = projected_a["wall_point"]
        point_b = projected_b["wall_point"]
        direction_uv = np.array(
            [
                float(np.dot(point_b - point_a, wall.u_axis)),
                float(np.dot(point_b - point_a, wall.v_axis)),
            ]
        )
        length = float(np.linalg.norm(direction_uv))
        if length < 1e-9:
            direction_uv = np.array([1.0, 0.0])
        else:
            direction_uv /= length
    perpendicular_uv = np.array([-direction_uv[1], direction_uv[0]])

    center_point = center_projection["wall_point"]
    center_screen = camera.project_point(center_point)
    if center_screen is None:
        result["status"] = "not_visible"
        return result
    neighbor_point = (
        center_point
        + wall.u_axis * (perpendicular_uv[0] * 0.08)
        + wall.v_axis * (perpendicular_uv[1] * 0.08)
    )
    neighbor_screen = camera.project_point(neighbor_point)
    if neighbor_screen is None:
        result["status"] = "not_visible"
        return result
    screen_direction = np.array(
        [
            neighbor_screen[0] - center_screen[0],
            neighbor_screen[1] - center_screen[1],
        ]
    )
    screen_length = float(np.linalg.norm(screen_direction))
    if screen_length < 1e-9:
        result["status"] = "degenerate_scan"
        return result
    screen_direction /= screen_length

    spacing = scene.shadows["edge_scan_spacing_px"]
    max_pixels = scene.shadows["edge_scan_max_px"]
    sample_offsets = np.arange(-max_pixels, max_pixels + 0.5 * spacing, spacing)
    samples = light_disk_samples(
        light,
        wall,
        scene.shadows["samples_per_light"],
    )
    brightness_values: list[float] = []
    visibility_values: list[float] = []
    inside_values: list[bool] = []
    wall_uv_values: list[list[float] | None] = []
    for offset in sample_offsets:
        screen_x = center_screen[0] + float(offset) * screen_direction[0]
        screen_y = center_screen[1] + float(offset) * screen_direction[1]
        direction = camera.ray_at_pixel(screen_x, screen_y)
        point, uv_value = _intersect_single_ray_with_wall(camera, direction, wall)
        if point is None:
            visibility = 1.0
            brightness = 1.0
            inside = False
            wall_uv_values.append(None)
        else:
            visibility = float(
                visibility_fraction(
                    point[None, :],
                    samples,
                    tassel.segments,
                )[0]
            )
            brightness = 0.16 + 0.84 * visibility
            inside = True
            assert uv_value is not None
            wall_uv_values.append([_f(uv_value[0]), _f(uv_value[1])])
        brightness_values.append(brightness)
        visibility_values.append(visibility)
        inside_values.append(inside)

    result["samples"] = [
        {
            "offset_px": _f(float(sample_offsets[index]), 4),
            "wall_uv": wall_uv_values[index],
            "inside_wall": inside_values[index],
            "visibility": _f(visibility_values[index], 6),
            "brightness": _f(brightness_values[index], 6),
        }
        for index in range(len(sample_offsets))
    ]
    values = np.asarray(brightness_values, dtype=np.float64)
    offsets = np.asarray(sample_offsets, dtype=np.float64)
    low = float(np.min(values))
    high = float(np.max(values))
    result["shadowed_brightness"] = _f(low, 6)
    result["unshadowed_brightness"] = _f(high, 6)
    widths: list[float] = []
    side_values: dict[str, float | None] = {
        "left_width_px": None,
        "right_width_px": None,
    }
    if high - low >= 0.08:
        low_level = low + 0.10 * (high - low)
        high_level = low + 0.90 * (high - low)
        for side_name, selector in (
            (
                "left_width_px",
                np.where(offsets <= 0.0)[0],
            ),
            (
                "right_width_px",
                np.where(offsets >= 0.0)[0],
            ),
        ):
            indices = selector
            distances = np.abs(offsets[indices])
            side_brightness = values[indices]
            order = np.argsort(distances, kind="stable")
            distances = distances[order]
            side_brightness = side_brightness[order]
            low_crossing = _crossing_distance(
                distances, side_brightness, low_level
            )
            high_crossing = _crossing_distance(
                distances, side_brightness, high_level
            )
            if low_crossing is not None and high_crossing is not None:
                width = max(0.0, high_crossing - low_crossing)
                side_values[side_name] = _f(width, 4)
                widths.append(width)
    result.update(side_values)
    if widths:
        result["transition_width_px"] = _f(
            float(np.mean(np.asarray(widths, dtype=np.float64))),
            4,
        )
    return result


def shadow_measurements(scene: SceneData, camera: Camera) -> list[dict[str, Any]]:
    tassel_by_id = {tassel.tassel_id: tassel for tassel in scene.tassels}
    light_by_id = {light.light_id: light for light in scene.lights}
    measurements: list[dict[str, Any]] = []
    for descriptor in scene.shadows["measurements"]:
        tassel_id = descriptor["tassel_id"]
        light_id = descriptor["light_id"]
        tassel = tassel_by_id[tassel_id]
        light = light_by_id[light_id]
        measurements.append(_soft_edge_measurement(scene, camera, tassel, light))
    measurements.sort(
        key=lambda measurement: (
            measurement["tassel_id"],
            measurement["light_id"],
        )
    )
    return measurements


def projection_rule_text() -> str:
    return (
        "wall_point = light.position + t * (source_point - light.position); "
        "t = dot(wall.origin - light.position, wall.normal) / "
        "dot(source_point - light.position, wall.normal)"
    )

