from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .scene import Camera, Layer, Light, Scene, Screen
from .util import stable_float, stable_list


@dataclass(frozen=True)
class Receiver:
    key: str
    kind: str
    origin: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray
    normal: np.ndarray
    width: float
    height: float
    layer_index: int | None = None

    def normalized(self, points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        relative = np.asarray(points, dtype=np.float64).reshape(-1, 3) - self.origin
        return (
            (relative @ self.axis_u) / self.width + 0.5,
            (relative @ self.axis_v) / self.height + 0.5,
        )


@dataclass(frozen=True)
class ProjectionCase:
    light_index: int
    light_id: str
    layer_index: int
    cutout_id: int
    triangle_index: int
    receiver_key: str
    receiver_kind: str
    status: str
    in_bounds: bool
    vertices_world: np.ndarray
    vertices_pixel: np.ndarray
    center_world: np.ndarray
    center_pixel: np.ndarray
    center_uv: np.ndarray
    penumbra_world: float
    radius_normalized_penumbra_world: float
    halfwidth_px: float
    projection_samples: int
    supersampling: int
    light_distance: float
    receiver_gap: float
    color_linear: np.ndarray

    def to_dict(self) -> dict[str, Any]:
        return {
            "light_id": self.light_id,
            "layer_index": int(self.layer_index),
            "cutout_id": int(self.cutout_id),
            "triangle_index": int(self.triangle_index),
            "receiver": self.receiver_key,
            "receiver_kind": self.receiver_kind,
            "status": self.status,
            "in_bounds": bool(self.in_bounds),
            "contributes": bool(self.in_bounds and self.status == "ok"),
            "vertices_world": [stable_list(vertex, 6) for vertex in self.vertices_world],
            "vertices_pixel": [stable_list(vertex, 4) for vertex in self.vertices_pixel]
            if self.vertices_pixel.size
            else [],
            "center_world": stable_list(self.center_world, 6),
            "center_pixel": stable_list(self.center_pixel, 4) if self.center_pixel.size else [],
            "receiver_uv_center": stable_list(self.center_uv, 6) if self.center_uv.size else [],
            "penumbra_width_output_px": stable_float(
                2.0 * self.halfwidth_px / max(self.supersampling, 1),
                6,
            ),
            "penumbra_width_supersampled_px": stable_float(2.0 * self.halfwidth_px, 6),
            "penumbra_width_world": stable_float(self.penumbra_world, 6),
            "radius_normalized_penumbra_width_world": stable_float(
                self.radius_normalized_penumbra_world,
                6,
            ),
            "projection_samples": int(self.projection_samples),
            "supersampling": int(self.supersampling),
            "light_distance": stable_float(self.light_distance, 6),
            "receiver_gap": stable_float(self.receiver_gap, 6),
        }


@dataclass
class ProjectionData:
    cases: tuple[ProjectionCase, ...]
    receiver_maps: dict[str, np.ndarray]
    light_maps: dict[tuple[str, int], np.ndarray]
    light_coverage: dict[tuple[str, int], np.ndarray]
    screen_light_coverage: np.ndarray
    screen_size: tuple[int, int]

    def receiver_map(self, key: str) -> np.ndarray | None:
        return self.receiver_maps.get(key)

    def report_dict(self, scene: Scene) -> dict[str, Any]:
        lights: list[dict[str, Any]] = []
        for light_index, light in enumerate(scene.lights):
            light_cases = [case for case in self.cases if case.light_index == light_index]
            lights.append(
                {
                    "light_id": light.light_id,
                    "position": stable_list(light.position, 6),
                    "intensity": stable_float(light.intensity, 6),
                    "projections": [case.to_dict() for case in light_cases],
                }
            )
        return {
            "schema_version": 1,
            "projection_samples": scene.render.projection_samples,
            "pixel_space": "supersampled camera image; (0,0) is the top-left sample cell",
            "supersampled_width": self.screen_size[0],
            "supersampled_height": self.screen_size[1],
            "lights": lights,
        }


def _screen_receiver(screen: Screen) -> Receiver:
    center = screen.origin + 0.5 * screen.width * screen.axis_u + 0.5 * screen.height * screen.axis_v
    return Receiver(
        key="screen",
        kind="screen",
        origin=center,
        axis_u=screen.axis_u,
        axis_v=screen.axis_v,
        normal=screen.normal,
        width=screen.width,
        height=screen.height,
    )


def _layer_receiver(layer: Layer) -> Receiver:
    plane = layer.plane
    return Receiver(
        key=f"layer:{layer.layer_index}",
        kind="layer",
        origin=plane.origin,
        axis_u=plane.axis_u,
        axis_v=plane.axis_v,
        normal=plane.normal,
        width=max(plane.bounds_u[1] - plane.bounds_u[0], 1e-9),
        height=max(plane.bounds_v[1] - plane.bounds_v[0], 1e-9),
        layer_index=layer.layer_index,
    )


def _world_to_pixel(
    points: np.ndarray,
    camera: Camera,
    width2: int,
    height2: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    pts = np.asarray(points, dtype=np.float64).reshape(-1, 3)
    relative = pts - camera.position
    depth = relative @ camera.forward
    safe = np.where(depth > camera.near, depth, np.nan)
    aspect = width2 / max(height2, 1)
    x = (relative @ camera.right) / safe
    y = (relative @ camera.camera_up) / safe
    px = ((x / (aspect * camera.tan_half_fov)) + 1.0) * 0.5 * width2
    py = (1.0 - y / camera.tan_half_fov) * 0.5 * height2
    return np.stack([px, py], axis=-1), depth, np.isfinite(px)


def _project_ray_to_plane(
    light_position: np.ndarray,
    occluder_points: np.ndarray,
    receiver: Receiver,
) -> tuple[np.ndarray, np.ndarray]:
    points = np.asarray(occluder_points, dtype=np.float64)
    origins = np.broadcast_to(light_position, points.shape)
    directions = points - origins
    denominator = directions @ receiver.normal
    plane_distance = float(np.dot(receiver.origin - light_position, receiver.normal))
    valid = np.abs(denominator) > 1e-12
    t = np.full(len(points), np.nan, dtype=np.float64)
    t[valid] = plane_distance / denominator[valid]
    valid &= np.isfinite(t) & (t > 1.0 + 1e-9)
    projected = origins + t[:, None] * directions
    projected[~valid] = np.nan
    return projected, valid


def _penumbra_world(
    light: Light,
    light_distance: float,
    receiver_gap: float,
) -> float:
    # Artistic scattering model: unlike an ideal point-source hard shadow, the
    # finite flame and participating smoke keep broadening with propagation.
    return (
        light.radius
        * (1.0 + 0.20 * max(light_distance, 0.0))
        * (1.0 + 2.2 * max(receiver_gap, 0.0))
        * 0.28
        * max(light.softness_gain, 0.0)
    )


def _segment_distance_2d(
    x: np.ndarray,
    y: np.ndarray,
    a: np.ndarray,
    b: np.ndarray,
) -> np.ndarray:
    ab = b - a
    denominator = float(np.dot(ab, ab))
    if denominator <= 1e-20:
        return np.hypot(x - a[0], y - a[1])
    t = np.clip(((x - a[0]) * ab[0] + (y - a[1]) * ab[1]) / denominator, 0.0, 1.0)
    return np.hypot(x - (a[0] + t * ab[0]), y - (a[1] + t * ab[1]))


def _signed_triangle_distance(
    x: np.ndarray,
    y: np.ndarray,
    triangle: np.ndarray,
) -> np.ndarray:
    a = triangle[0]
    b = triangle[1]
    c = triangle[2]
    ba = b - a
    ca = c - a
    area2 = float(ba[0] * ca[1] - ba[1] * ca[0])
    if abs(area2) <= 1e-12:
        return np.full(np.broadcast(x, y).shape, -1e9, dtype=np.float64)
    if area2 < 0.0:
        b, c = c, b

    def edge_value(p: np.ndarray, q: np.ndarray) -> np.ndarray:
        return (q[0] - p[0]) * (y - p[1]) - (q[1] - p[1]) * (x - p[0])

    e0 = edge_value(a, b)
    e1 = edge_value(b, c)
    e2 = edge_value(c, a)
    inside = (e0 >= 0.0) & (e1 >= 0.0) & (e2 >= 0.0)
    len0 = max(float(np.linalg.norm(b - a)), 1e-20)
    len1 = max(float(np.linalg.norm(c - b)), 1e-20)
    len2 = max(float(np.linalg.norm(a - c)), 1e-20)
    inside_distance = np.minimum.reduce([e0 / len0, e1 / len1, e2 / len2])
    outside_distance = np.minimum.reduce(
        [
            _segment_distance_2d(x, y, a, b),
            _segment_distance_2d(x, y, b, c),
            _segment_distance_2d(x, y, c, a),
        ]
    )
    return np.where(inside, inside_distance, -outside_distance)


def _smoothstep(value: np.ndarray) -> np.ndarray:
    t = np.clip(value, 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def _coverage_on_bbox(
    triangle: np.ndarray,
    x0: int,
    x1: int,
    y0: int,
    y1: int,
    halfwidth_px: float,
    samples: int,
) -> np.ndarray:
    xs = np.arange(x0, x1, dtype=np.float64) + 0.5
    ys = np.arange(y0, y1, dtype=np.float64) + 0.5
    xx, yy = np.meshgrid(xs, ys)
    accumulated = np.zeros_like(xx, dtype=np.float64)
    offsets = (np.arange(samples, dtype=np.float64) + 0.5) / samples - 0.5
    halfwidth = max(halfwidth_px, 1e-4)
    for offset_y in offsets:
        for offset_x in offsets:
            signed = _signed_triangle_distance(xx + offset_x, yy + offset_y, triangle)
            accumulated += _smoothstep((signed + halfwidth) / (2.0 * halfwidth))
    return accumulated / float(samples * samples)


def _luminance(color: np.ndarray) -> float:
    return float(np.dot(color, np.array([0.2126, 0.7152, 0.0722])))


def build_projection_data(scene: Scene) -> ProjectionData:
    width2 = scene.width * scene.render.supersampling
    height2 = scene.height * scene.render.supersampling
    screen_receiver = _screen_receiver(scene.screen)
    layer_receivers = {layer.layer_index: _layer_receiver(layer) for layer in scene.layers}
    all_receivers: dict[str, Receiver] = {"screen": screen_receiver}
    for layer in scene.layers[1:]:
        receiver = layer_receivers[layer.layer_index]
        all_receivers[receiver.key] = receiver

    cases: list[ProjectionCase] = []
    for layer in scene.layers:
        outer_layer = next(
            (candidate for candidate in scene.layers if candidate.layer_index > layer.layer_index),
            None,
        )
        for cutout_id in sorted(layer.cutouts):
            triangle_indices = layer.cutouts[cutout_id]
            source_triangles = [
                (triangle_index, layer.triangles[triangle_index])
                for triangle_index in triangle_indices
            ]
            cutout_points: list[np.ndarray] = []
            for _, triangle in source_triangles:
                cutout_points.extend(layer.vertices[triangle])
            centroid = np.mean(np.asarray(cutout_points, dtype=np.float64), axis=0)

            receivers: list[Receiver] = []
            if outer_layer is not None:
                receivers.append(layer_receivers[outer_layer.layer_index])
            receivers.append(screen_receiver)

            for light_index, light in enumerate(scene.lights):
                light_distance = float(np.linalg.norm(centroid - light.position))
                for receiver in receivers:
                    receiver_gap = abs(float(np.dot(receiver.origin - centroid, receiver.normal)))
                    penumbra_world = _penumbra_world(light, light_distance, receiver_gap)
                    to_light = light.position - centroid
                    to_light_length = max(float(np.linalg.norm(to_light)), 1e-30)
                    incidence = abs(float(np.dot(layer.plane.normal, to_light / to_light_length)))
                    attenuation = light.intensity / (1.0 + 0.55 * light_distance**2)
                    aperture_tint = np.clip(0.55 * layer.tint + 0.45, 0.0, 1.0)
                    projection_color = (
                        light.color
                        * aperture_tint
                        * attenuation
                        * (0.30 + 0.70 * incidence)
                        * 0.78
                    )
                    for triangle_index, _ in source_triangles:
                        world_triangle = layer.vertices[layer.triangles[triangle_index]]
                        projected, valid = _project_ray_to_plane(
                            light.position,
                            world_triangle,
                            receiver,
                        )
                        status = "ok" if bool(np.all(valid)) else "behind_light_or_parallel"
                        pixel_triangle = np.zeros((0, 2), dtype=np.float64)
                        center_pixel = np.zeros((0,), dtype=np.float64)
                        center_uv = np.zeros((0,), dtype=np.float64)
                        in_bounds = False
                        halfwidth_px = 0.0
                        if bool(np.all(valid)):
                            pixels, depth, pixel_valid = _world_to_pixel(
                                projected,
                                scene.camera,
                                width2,
                                height2,
                            )
                            if not bool(np.all(pixel_valid)):
                                status = "behind_camera"
                            else:
                                pixel_triangle = pixels
                                u, v = receiver.normalized(projected)
                                uv = np.stack([u, v], axis=-1)
                                receiver_inside = (u >= 0.0) & (u <= 1.0) & (v >= 0.0) & (v <= 1.0)
                                in_bounds = bool(np.any(receiver_inside))
                                center_world = np.mean(projected, axis=0)
                                center_pixel_array, _, center_valid = _world_to_pixel(
                                    center_world[None, :],
                                    scene.camera,
                                    width2,
                                    height2,
                                )
                                if bool(center_valid[0]):
                                    center_pixel = center_pixel_array[0]
                                center_uv = np.mean(uv, axis=0)
                                world_edges = np.asarray(
                                    [
                                        np.linalg.norm(world_triangle[1] - world_triangle[0]),
                                        np.linalg.norm(world_triangle[2] - world_triangle[1]),
                                        np.linalg.norm(world_triangle[0] - world_triangle[2]),
                                    ]
                                )
                                pixel_edges = np.asarray(
                                    [
                                        np.linalg.norm(pixels[1] - pixels[0]),
                                        np.linalg.norm(pixels[2] - pixels[1]),
                                        np.linalg.norm(pixels[0] - pixels[2]),
                                    ]
                                )
                                valid_edges = world_edges > 1e-12
                                scale = (
                                    float(np.mean(pixel_edges[valid_edges] / world_edges[valid_edges]))
                                    if bool(np.any(valid_edges))
                                    else 1.0
                                )
                                halfwidth_px = max(0.25, 0.5 * penumbra_world * scale)
                                if not in_bounds:
                                    status = "outside_receiver"
                        if status != "ok":
                            in_bounds = False
                        cases.append(
                            ProjectionCase(
                                light_index=light_index,
                                light_id=light.light_id,
                                layer_index=layer.layer_index,
                                cutout_id=cutout_id,
                                triangle_index=triangle_index,
                                receiver_key=receiver.key,
                                receiver_kind=receiver.kind,
                                status=status,
                                in_bounds=in_bounds,
                                vertices_world=(
                                    projected if bool(np.all(valid)) else world_triangle
                                ),
                                vertices_pixel=pixel_triangle,
                                center_world=(
                                    np.mean(projected, axis=0)
                                    if bool(np.all(valid))
                                    else centroid
                                ),
                                center_pixel=center_pixel,
                                center_uv=center_uv,
                                penumbra_world=penumbra_world,
                                radius_normalized_penumbra_world=(
                                    penumbra_world
                                    / max(light.radius * max(light.softness_gain, 1e-12), 1e-12)
                                ),
                                halfwidth_px=halfwidth_px,
                                projection_samples=scene.render.projection_samples,
                                supersampling=scene.render.supersampling,
                                light_distance=light_distance,
                                receiver_gap=receiver_gap,
                                color_linear=projection_color,
                            )
                        )

    light_maps: dict[tuple[str, int], np.ndarray] = {}
    light_coverage: dict[tuple[str, int], np.ndarray] = {}
    grouped: dict[tuple[str, int, int, int], list[ProjectionCase]] = {}
    for case in cases:
        if case.status == "ok" and case.in_bounds:
            key = (case.receiver_key, case.light_index, case.layer_index, case.cutout_id)
            grouped.setdefault(key, []).append(case)

    for (receiver_key, light_index, layer_index, cutout_id), group_cases in grouped.items():
        rgb_key = (receiver_key, light_index)
        if rgb_key not in light_maps:
            light_maps[rgb_key] = np.zeros((height2, width2, 3), dtype=np.float32)
            light_coverage[rgb_key] = np.zeros((height2, width2), dtype=np.float32)

        halfwidth = max(case.halfwidth_px for case in group_cases)
        all_pixels = np.concatenate([case.vertices_pixel for case in group_cases], axis=0)
        min_x = max(0, int(np.floor(float(np.min(all_pixels[:, 0])) - halfwidth - 1.0)))
        max_x = min(width2, int(np.ceil(float(np.max(all_pixels[:, 0])) + halfwidth + 2.0)))
        min_y = max(0, int(np.floor(float(np.min(all_pixels[:, 1])) - halfwidth - 1.0)))
        max_y = min(height2, int(np.ceil(float(np.max(all_pixels[:, 1])) + halfwidth + 2.0)))
        if max_x <= min_x or max_y <= min_y:
            continue
        union_coverage = np.zeros((max_y - min_y, max_x - min_x), dtype=np.float64)
        samples = scene.render.projection_samples
        for case in group_cases:
            tri = case.vertices_pixel
            tri_min_x = max(min_x, int(np.floor(float(np.min(tri[:, 0])) - case.halfwidth_px - 1.0)))
            tri_max_x = min(max_x, int(np.ceil(float(np.max(tri[:, 0])) + case.halfwidth_px + 2.0)))
            tri_min_y = max(min_y, int(np.floor(float(np.min(tri[:, 1])) - case.halfwidth_px - 1.0)))
            tri_max_y = min(max_y, int(np.ceil(float(np.max(tri[:, 1])) + case.halfwidth_px + 2.0)))
            if tri_max_x <= tri_min_x or tri_max_y <= tri_min_y:
                continue
            local = _coverage_on_bbox(
                tri,
                tri_min_x,
                tri_max_x,
                tri_min_y,
                tri_max_y,
                case.halfwidth_px,
                samples,
            )
            offset_y = tri_min_y - min_y
            offset_x = tri_min_x - min_x
            target = union_coverage[
                offset_y : offset_y + local.shape[0],
                offset_x : offset_x + local.shape[1],
            ]
            np.maximum(target, local, out=target)

        color = group_cases[0].color_linear
        light_maps[rgb_key][min_y:max_y, min_x:max_x] += (
            union_coverage[..., None] * color[None, None, :]
        ).astype(np.float32)
        coverage_target = light_coverage[rgb_key][min_y:max_y, min_x:max_x]
        np.maximum(coverage_target, union_coverage.astype(np.float32), out=coverage_target)

    receiver_maps: dict[str, np.ndarray] = {}
    for (receiver_key, _), light_map in sorted(light_maps.items()):
        if receiver_key not in receiver_maps:
            receiver_maps[receiver_key] = np.zeros((height2, width2, 3), dtype=np.float32)
        receiver_maps[receiver_key] += light_map

    screen_light_coverage = np.zeros((len(scene.lights), height2, width2), dtype=np.float32)
    for light_index in range(len(scene.lights)):
        coverage = light_coverage.get(("screen", light_index))
        if coverage is not None:
            screen_light_coverage[light_index] = coverage

    return ProjectionData(
        cases=tuple(cases),
        receiver_maps=receiver_maps,
        light_maps=light_maps,
        light_coverage=light_coverage,
        screen_light_coverage=screen_light_coverage,
        screen_size=(width2, height2),
    )


def penumbra_stats(projection: ProjectionData, scene: Scene) -> dict[str, Any]:
    comparisons: list[dict[str, Any]] = []
    for light_index, light in enumerate(scene.lights):
        for layer in scene.layers:
            adjacent_index = next(
                (candidate.layer_index for candidate in scene.layers if candidate.layer_index > layer.layer_index),
                None,
            )
            if adjacent_index is None:
                continue
            for cutout_id in sorted(layer.cutouts):
                adjacent = next(
                    (
                        case
                        for case in projection.cases
                        if case.light_index == light_index
                        and case.layer_index == layer.layer_index
                        and case.cutout_id == cutout_id
                        and case.receiver_key == f"layer:{adjacent_index}"
                        and case.status == "ok"
                    ),
                    None,
                )
                screen = next(
                    (
                        case
                        for case in projection.cases
                        if case.light_index == light_index
                        and case.layer_index == layer.layer_index
                        and case.cutout_id == cutout_id
                        and case.receiver_key == "screen"
                        and case.status == "ok"
                    ),
                    None,
                )
                if adjacent is None or screen is None:
                    continue
                supersampling = max(scene.render.supersampling, 1)
                adjacent_width = 2.0 * adjacent.halfwidth_px / supersampling
                screen_width = 2.0 * screen.halfwidth_px / supersampling
                profile_offsets = [-1.0, -0.5, 0.0, 0.5, 1.0]
                profiles = []
                for case in (adjacent, screen):
                    halfwidth = max(case.halfwidth_px / supersampling, 1e-6)
                    samples = []
                    for offset in profile_offsets:
                        signed = offset * halfwidth
                        coverage = float(_smoothstep(np.array((signed + halfwidth) / (2.0 * halfwidth))))
                        samples.append(
                            {
                                "offset_px": stable_float(signed, 6),
                                "coverage": stable_float(coverage, 6),
                                "relative_linear_luminance": stable_float(
                                    coverage * _luminance(case.color_linear),
                                    6,
                                ),
                            }
                        )
                    profiles.append(
                        {
                            "receiver": case.receiver_key,
                            "receiver_gap": stable_float(case.receiver_gap, 6),
                            "light_distance": stable_float(case.light_distance, 6),
                            "penumbra_width_world": stable_float(case.penumbra_world, 6),
                            "penumbra_width_output_px": stable_float(2.0 * halfwidth, 6),
                            "transition_10_90_output_px": stable_float(1.6 * halfwidth, 6),
                            "samples": samples,
                        }
                    )
                comparisons.append(
                    {
                        "light_id": light.light_id,
                        "layer_index": layer.layer_index,
                        "cutout_id": cutout_id,
                        "adjacent_width_output_px": stable_float(adjacent_width, 6),
                        "screen_width_output_px": stable_float(screen_width, 6),
                        "broader_at_screen": bool(screen_width >= adjacent_width),
                        "profiles": profiles,
                    }
                )
    grouped_cases: dict[tuple[int, int, int, str], ProjectionCase] = {}
    for case in projection.cases:
        if case.status != "ok":
            continue
        grouped_cases.setdefault(
            (case.light_index, case.layer_index, case.cutout_id, case.receiver_key),
            case,
        )
    by_distance: dict[tuple[int, int, str], list[tuple[float, float]]] = {}
    for (light_index, layer_index, cutout_id, receiver_key), case in grouped_cases.items():
        by_distance.setdefault((layer_index, cutout_id, receiver_key), []).append(
            (
                case.light_distance,
                case.penumbra_world,
                case.radius_normalized_penumbra_world,
            )
        )
    distance_trend_raw_ok = True
    distance_trend_normalized_ok = True
    distance_groups = 0
    for values in by_distance.values():
        values = sorted(values)
        distance_groups += 1
        widths = [width for _, width, _ in values]
        normalized = [width for _, _, width in values]
        distance_trend_raw_ok &= all(
            widths[index] <= widths[index + 1] + 1e-12 for index in range(len(widths) - 1)
        )
        distance_trend_normalized_ok &= all(
            normalized[index] <= normalized[index + 1] + 1e-12
            for index in range(len(normalized) - 1)
        )
    gap_pairs_ok = all(item["broader_at_screen"] for item in comparisons) if comparisons else True
    return {
        "model": "finite-flame-participating-scatter-v1",
        "projection_samples": scene.render.projection_samples,
        "width_units": "output pixels after supersample reduction",
        "trend_checks": {
            "receiver_gap_pairs_checked": len(comparisons),
            "receiver_gap_non_decreasing": bool(gap_pairs_ok),
            "light_distance_groups_checked": distance_groups,
            "light_distance_non_decreasing_raw_world_width": bool(distance_trend_raw_ok),
            "light_distance_non_decreasing_radius_normalized_width": bool(
                distance_trend_normalized_ok
            ),
        },
        "comparisons": comparisons,
    }

