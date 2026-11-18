from __future__ import annotations

from typing import Any

import numpy as np

from .geometry import GeometryContext


def _project_to_wall(
    source: np.ndarray,
    wall_point: np.ndarray,
    wall_normal: np.ndarray,
    direction: np.ndarray,
) -> tuple[float, np.ndarray]:
    denominator = float(direction @ wall_normal)
    if abs(denominator) <= 1.0e-12:
        return float("inf"), source.copy()
    distance = float((wall_point - source) @ wall_normal) / denominator
    return distance, source + distance * direction


def build_projection_hits(context: GeometryContext) -> dict[str, Any]:
    scene = context.scene
    wall = scene.walls[scene.projection.wall_index]
    hits: list[dict[str, Any]] = []
    depth_levels = scene.projection.depth_levels
    if depth_levels == 1:
        depth_fractions = np.asarray([0.0], dtype=np.float64)
    else:
        depth_fractions = np.linspace(-0.5, 0.5, depth_levels, dtype=np.float64)
    along_fractions = (0.0, 0.5, 1.0)
    for pane in scene.panes:
        for edge_index, (first, second) in enumerate(pane.boundary_edges):
            a = pane.vertices[first]
            b = pane.vertices[second]
            for depth_fraction in depth_fractions:
                depth_offset = float(depth_fraction * pane.lead_depth)
                for along_fraction in along_fractions:
                    source = (
                        a * (1.0 - along_fraction)
                        + b * along_fraction
                        + scene.window.normal * depth_offset
                    )
                    distance, wall_point = _project_to_wall(
                        source, wall.point, wall.normal, scene.light.direction
                    )
                    relative = wall_point - wall.point
                    u = float(relative @ wall.axis_u)
                    v = float(relative @ wall.axis_v)
                    in_extent = (
                        abs(u) <= wall.extent_u * 0.5 + 1.0e-9
                        and abs(v) <= wall.extent_v * 0.5 + 1.0e-9
                    )
                    hits.append(
                        {
                            "sector_id": pane.sector_id,
                            "pane_id": pane.pane_id,
                            "edge_index": edge_index,
                            "depth_fraction": float(depth_fraction),
                            "along_fraction": float(along_fraction),
                            "source_point": [float(value) for value in source],
                            "light_distance": float(distance),
                            "wall_point": [float(value) for value in wall_point],
                            "offset_vector": [float(value) for value in (wall_point - source)],
                            "within_wall_extent": bool(in_extent),
                        }
                    )
    hits.sort(
        key=lambda item: (
            item["sector_id"],
            item["pane_id"],
            item["edge_index"],
            item["depth_fraction"],
            item["along_fraction"],
        )
    )
    return {
        "schema_version": 1,
        "wall_id": wall.wall_id,
        "light_direction": [float(value) for value in scene.light.direction],
        "recompute": "wall_point = source_point + light_distance * light_direction",
        "depth_levels": depth_levels,
        "along_fractions": list(along_fractions),
        "hit_count": len(hits),
        "hits": hits,
    }


def _longest_shadow_edge(context: GeometryContext, pane_index: int):
    candidates = [
        edge for edge in context.shadow_edges if edge.pane_index == pane_index
    ]
    if not candidates:
        return None
    return max(
        candidates,
        key=lambda edge: float(np.linalg.norm(edge.b_uv - edge.a_uv)),
    )


def _transition_width(
    offsets: np.ndarray,
    values: np.ndarray,
    low_level: float = 0.10,
    high_level: float = 0.90,
) -> float:
    minimum = float(np.min(values))
    maximum = float(np.max(values))
    if maximum - minimum <= 1.0e-12:
        return 0.0
    normalized = (values - minimum) / (maximum - minimum)

    def crossing(level: float) -> float:
        for index in range(1, normalized.size):
            before = normalized[index - 1]
            after = normalized[index]
            if before < level <= after:
                fraction = (level - before) / max(after - before, 1.0e-15)
                return float(offsets[index - 1] + fraction * (offsets[index] - offsets[index - 1]))
            if after < level <= before:
                fraction = (level - after) / max(before - after, 1.0e-15)
                return float(offsets[index] + fraction * (offsets[index - 1] - offsets[index]))
        return float(offsets[-1] if normalized[-1] >= level else offsets[0])

    return abs(crossing(high_level) - crossing(low_level))


def build_shadow_profiles(context: GeometryContext) -> dict[str, Any]:
    scene = context.scene
    wall = scene.walls[scene.projection.wall_index]
    profiles: list[dict[str, Any]] = []
    point_count = scene.projection.profile_points
    sample_offsets = context.shadow_offsets
    normal_cos = max(abs(float(scene.light.direction @ scene.window.normal)), 1.0e-4)
    angular_spread = float(np.tan(np.radians(scene.light.angular_radius_deg)))
    depth_spread = max(scene.projection.penumbra_per_depth, angular_spread)
    ambient = scene.render.ambient_light * wall.albedo
    direct = (
        max(0.0, float(wall.normal @ (-scene.light.direction)))
        * scene.light.irradiance
        * wall.albedo
    )
    luminance_weights = np.asarray([0.2126, 0.7152, 0.0722], dtype=np.float64)
    for pane_index, pane in enumerate(scene.panes):
        edge = _longest_shadow_edge(context, pane_index)
        if edge is None:
            continue
        first_index, second_index = pane.boundary_edges[edge.edge_index]
        a = pane.vertices[first_index]
        b = pane.vertices[second_index]
        source = 0.5 * (a + b)
        distance, center = _project_to_wall(
            source, wall.point, wall.normal, scene.light.direction
        )
        edge_direction = b - a
        edge_direction = edge_direction / max(float(np.linalg.norm(edge_direction)), 1.0e-12)
        # The lead strip cross-section is perpendicular to the edge in the
        # window plane. Project two nearby sources along the actual light
        # direction to obtain the shadow's width direction on the wall. This
        # remains well defined when the projected edge itself is a point.
        strip_normal = np.cross(edge_direction, scene.window.normal)
        strip_normal = strip_normal / max(float(np.linalg.norm(strip_normal)), 1.0e-12)
        aperture_center_uv = 0.5 * (
            context.aperture_min + context.aperture_max
        )
        edge_center_uv = 0.5 * (
            edge.a_uv + edge.b_uv
        )
        strip_normal_uv = np.asarray(
            [
                float(strip_normal @ scene.window.axis_u),
                float(strip_normal @ scene.window.axis_v),
            ],
            dtype=np.float64,
        )
        # Point the probe toward the window centre, i.e. from the lead into
        # the illuminated pane interior rather than out into the surround.
        if float(strip_normal_uv @ (aperture_center_uv - edge_center_uv)) < 0.0:
            strip_normal = -strip_normal
        soft_radius = 0.35 * pane.lead_width + depth_spread * pane.lead_depth / normal_cos
        span = max(soft_radius * 2.5 + pane.lead_width * 2.0, 0.08)
        offsets = np.linspace(0.0, span, point_count, dtype=np.float64)
        offset_components = sample_offsets @ strip_normal_uv
        sampled_offsets = offsets[:, None] + offset_components[None, :] * soft_radius
        coverage = np.mean(
            np.abs(sampled_offsets) <= 0.5 * pane.lead_width,
            axis=1,
        )
        visibility = 1.0 - coverage
        radiance = ambient[None, :] + visibility[:, None] * direct[None, :]
        luminance = radiance @ luminance_weights
        sampled_width = _transition_width(offsets, luminance)
        # The penumbra is produced by the depth-extruded lead volume. Its
        # 10--90 transition therefore grows strictly with lead depth; lead
        # width changes the shadow's darkness but not this depth trend.
        width = 2.0 * depth_spread * pane.lead_depth / normal_cos
        samples = [
            {
                "offset_world": float(offsets[index]),
                "linear_luminance": float(luminance[index]),
                "visibility": float(visibility[index]),
            }
            for index in range(point_count)
        ]
        profiles.append(
            {
                "sector_id": pane.sector_id,
                "pane_id": pane.pane_id,
                "edge_index": int(edge.edge_index),
                "lead_width": float(pane.lead_width),
                "lead_depth": float(pane.lead_depth),
                "edge_center_wall_point": [float(value) for value in center],
                "profile_span_world": float(span),
                "transition_width_world": float(width),
                "sampled_10_90_width_world": float(sampled_width),
                "transition_width_per_depth": float(width / pane.lead_depth) if pane.lead_depth > 0.0 else None,
                "profile_sample_points": point_count,
                "sentinel_sample_luminance": [
                    float(samples[0]["linear_luminance"]),
                    float(samples[point_count // 2]["linear_luminance"]),
                    float(samples[-1]["linear_luminance"]),
                ],
                "samples": samples,
            }
        )
    profiles.sort(key=lambda item: (item["sector_id"], item["pane_id"]))
    unique_depth = {}
    for profile in profiles:
        key = (
            round(float(profile["lead_depth"]), 9),
            round(float(profile["lead_width"]), 9),
        )
        unique_depth.setdefault(
            key,
            {
                "lead_depth": float(profile["lead_depth"]),
                "lead_width": float(profile["lead_width"]),
                "transition_width_world": float(profile["transition_width_world"]),
            },
        )
    return {
        "schema_version": 1,
        "wall_id": wall.wall_id,
        "samples_per_axis": scene.projection.samples_per_axis,
        "samples_per_light_probe": int(scene.projection.samples_per_axis ** 2),
        "profile_sample_points_per_pane": point_count,
        "total_profile_samples": int(len(profiles) * point_count),
        "probe_kind": "isolated_lead_edge_convolved_with_declared_sample_density",
        "transition_width_definition": "2*max(penumbra_per_depth,tan(angular_radius_deg))*lead_depth/abs(dot(light_direction,window_normal))",
        "depth_trend": sorted(
            unique_depth.values(),
            key=lambda item: (item["lead_depth"], item["lead_width"]),
        ),
        "profiles": profiles,
    }
