from __future__ import annotations

import numpy as np

from .geometry import (
    GeometryContext,
    plane_intersection,
    point_segment_distance,
)
from .scene import Wall


def wall_sun_visibility(
    context: GeometryContext,
    wall: Wall,
    wall_points: np.ndarray,
) -> tuple[np.ndarray, dict[str, int]]:
    """Deterministic soft visibility of the window aperture from a wall point.

    The direction light fixes where the wall point projects into the window
    plane. The declared penumbra-per-depth factor convolves each lead strip
    with the sample pattern, so the transition widens monotonically with the
    depth of that pane's lead.
    """
    scene = context.scene
    points = np.asarray(wall_points, dtype=np.float64).reshape(-1, 3)
    if points.size == 0:
        return np.zeros(0, dtype=np.float64), {
            "wall_points": 0,
            "edge_probes": 0,
            "offset_samples_per_edge": int(context.shadow_offsets.shape[0]),
        }

    toward_light = -scene.light.direction
    distance, window_hit = plane_intersection(
        points, np.broadcast_to(toward_light, points.shape), scene.window.origin, scene.window.normal
    )
    relative = window_hit - scene.window.origin
    uv = np.column_stack(
        (relative @ scene.window.axis_u, relative @ scene.window.axis_v)
    )
    inside = np.all(
        (uv >= context.aperture_min[None, :] - 1.0e-9)
        & (uv <= context.aperture_max[None, :] + 1.0e-9),
        axis=1,
    )
    valid = (distance > 1.0e-7) & np.isfinite(distance) & inside

    occlusion = np.zeros(points.shape[0], dtype=np.float64)
    normal_cos = abs(float(scene.light.direction @ scene.window.normal))
    normal_cos = max(normal_cos, 1.0e-4)
    angular_spread = float(
        np.tan(np.radians(scene.light.angular_radius_deg))
    )
    depth_spread = max(scene.projection.penumbra_per_depth, angular_spread)
    for edge in context.shadow_edges:
        radius = (
            0.35 * edge.width
            + depth_spread * edge.depth / normal_cos
        )
        if radius <= 0.0:
            sampled_uv = uv
            distances = point_segment_distance(sampled_uv, edge.a_uv, edge.b_uv)
            coverage = (distances <= 0.5 * edge.width).astype(np.float64)
        else:
            sampled_uv = uv[:, None, :] + context.shadow_offsets[None, :, :] * radius
            distances = point_segment_distance(sampled_uv, edge.a_uv, edge.b_uv)
            coverage = np.mean(distances <= 0.5 * edge.width, axis=1)
        occlusion = np.maximum(occlusion, coverage)

    visibility = np.where(valid, 1.0 - occlusion, 0.0)
    sample_count = int(context.shadow_offsets.shape[0])
    metrics = {
        "wall_points": int(points.shape[0]),
        "edge_probes": int(points.shape[0] * len(context.shadow_edges)),
        "offset_samples_per_edge": sample_count,
        "sampled_offsets": int(points.shape[0] * len(context.shadow_edges) * sample_count),
    }
    return np.clip(visibility, 0.0, 1.0), metrics


def wall_linear_radiance(
    context: GeometryContext,
    wall: Wall,
    wall_points: np.ndarray,
) -> tuple[np.ndarray, dict[str, int]]:
    scene = context.scene
    points = np.asarray(wall_points, dtype=np.float64).reshape(-1, 3)
    if wall.receives_shadow:
        visibility, metrics = wall_sun_visibility(context, wall, points)
    else:
        visibility = np.zeros(points.shape[0], dtype=np.float64)
        metrics = {
            "wall_points": int(points.shape[0]),
            "edge_probes": 0,
            "offset_samples_per_edge": int(context.shadow_offsets.shape[0]),
            "sampled_offsets": 0,
        }
    cosine = max(0.0, float(wall.normal @ (-scene.light.direction)))
    direct = (
        visibility[:, None]
        * cosine
        * scene.light.irradiance[None, :]
        * wall.albedo[None, :]
    )
    ambient = scene.render.ambient_light[None, :] * wall.albedo[None, :]
    return direct + ambient, metrics
