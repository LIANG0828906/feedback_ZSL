"""Lead-bar projection and deterministic soft-shadow evaluation."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .geometry import LeadEdge
from .scene import Scene


@dataclass(frozen=True)
class ShadowResult:
    coverage: np.ndarray
    resolution: int
    min_x: float
    max_x: float
    min_y: float
    max_y: float
    projections: tuple[dict[str, Any], ...]
    profile: dict[str, Any]
    digest: str

    def sample(self, x: np.ndarray | float, y: np.ndarray | float) -> np.ndarray:
        x_array = np.asarray(x, dtype=np.float64)
        y_array = np.asarray(y, dtype=np.float64)
        gx = (x_array - self.min_x) / (self.max_x - self.min_x) * (self.resolution - 1)
        gy = (y_array - self.min_y) / (self.max_y - self.min_y) * (self.resolution - 1)
        gx = np.clip(gx, 0.0, self.resolution - 1.0)
        gy = np.clip(gy, 0.0, self.resolution - 1.0)
        x0 = np.floor(gx).astype(np.intp)
        y0 = np.floor(gy).astype(np.intp)
        x1 = np.minimum(x0 + 1, self.resolution - 1)
        y1 = np.minimum(y0 + 1, self.resolution - 1)
        fx = gx - x0
        fy = gy - y0
        top = (
            self.coverage[y0, x0] * (1.0 - fx)
            + self.coverage[y0, x1] * fx
        )
        bottom = (
            self.coverage[y1, x0] * (1.0 - fx)
            + self.coverage[y1, x1] * fx
        )
        return top * (1.0 - fy) + bottom * fy


def _shadow_extent(scene: Scene) -> tuple[float, float, float, float]:
    min_x = scene.wall.center_x - scene.wall.width * 0.5
    max_x = scene.wall.center_x + scene.wall.width * 0.5
    min_y = scene.wall.center_y - scene.wall.height * 0.5
    max_y = scene.wall.center_y + scene.wall.height * 0.5
    return min_x, max_x, min_y, max_y


def project_to_wall(
    point: np.ndarray, scene: Scene, direction: np.ndarray | None = None
) -> np.ndarray:
    ray = scene.light.direction if direction is None else direction
    denominator = float(ray[2])
    if abs(denominator) <= 1e-9:
        raise ValueError("light direction is parallel to the wall plane")
    t = (scene.wall.z - float(point[2])) / denominator
    return np.asarray(point[:2] + ray[:2] * t, dtype=np.float64)


def _penumbra(scene: Scene, depth: float, z_glass: float) -> float:
    distance = abs(scene.wall.z - z_glass)
    angular = 2.0 * distance * math.tan(scene.light.angular_radius)
    return max(0.0, angular + max(depth, 0.0) * scene.shadows.penumbra_depth_scale)


def _soft_cover(
    distance: np.ndarray,
    half_width: float,
    penumbra: float,
    samples: int,
) -> np.ndarray:
    if penumbra <= 1e-12:
        return (distance <= half_width).astype(np.float64)
    offsets = (
        (np.arange(samples, dtype=np.float64) + 0.5) / samples - 0.5
    ) * penumbra
    coverage = np.zeros_like(distance, dtype=np.float64)
    for offset in offsets:
        coverage += distance <= (half_width + offset)
    return coverage / samples


def _distance_to_segment(
    x: np.ndarray, y: np.ndarray, a: np.ndarray, b: np.ndarray
) -> np.ndarray:
    abx = float(b[0] - a[0])
    aby = float(b[1] - a[1])
    length_squared = abx * abx + aby * aby
    if length_squared <= 1e-20:
        return np.hypot(x - a[0], y - a[1])
    projection = ((x - a[0]) * abx + (y - a[1]) * aby) / length_squared
    projection = np.clip(projection, 0.0, 1.0)
    nearest_x = a[0] + projection * abx
    nearest_y = a[1] + projection * aby
    return np.hypot(x - nearest_x, y - nearest_y)


def _edge_coverage_at_points(
    points_xy: np.ndarray,
    edge: LeadEdge,
    scene: Scene,
    depth_override: float | None = None,
) -> np.ndarray:
    depth = edge.depth if depth_override is None else max(depth_override, 0.0)
    depth_steps = max(1, min(scene.shadows.samples, 8))
    z_offsets = (
        np.linspace(0.0, depth, depth_steps)
        if depth > 0.0
        else np.asarray([0.0], dtype=np.float64)
    )
    result = np.zeros(points_xy.shape[0], dtype=np.float64)
    for z_offset in z_offsets:
        a = edge.a.copy()
        b = edge.b.copy()
        a[2] += float(z_offset)
        b[2] += float(z_offset)
        projected_a = project_to_wall(a, scene)
        projected_b = project_to_wall(b, scene)
        distance = _distance_to_segment(
            points_xy[:, 0], points_xy[:, 1], projected_a, projected_b
        )
        penumbra = max(
            _penumbra(scene, depth, float(a[2])),
            (scene.wall.width / scene.shadows.resolution) * 1.25,
        )
        coverage = _soft_cover(
            distance, edge.width * 0.5, penumbra, scene.shadows.samples
        )
        result = np.maximum(result, coverage)
    return result


def _build_coverage_map(
    scene: Scene,
    edges: Iterable[LeadEdge],
    min_x: float,
    max_x: float,
    min_y: float,
    max_y: float,
) -> np.ndarray:
    resolution = scene.shadows.resolution
    coverage = np.zeros((resolution, resolution), dtype=np.float64)
    texel_x = (max_x - min_x) / max(resolution - 1, 1)
    texel_y = (max_y - min_y) / max(resolution - 1, 1)
    for edge in edges:
        depth = edge.depth
        depth_steps = max(1, min(scene.shadows.samples, 8))
        z_offsets = (
            np.linspace(0.0, depth, depth_steps)
            if depth > 0.0
            else np.asarray([0.0], dtype=np.float64)
        )
        for z_offset in z_offsets:
            a = edge.a.copy()
            b = edge.b.copy()
            a[2] += float(z_offset)
            b[2] += float(z_offset)
            projected_a = project_to_wall(a, scene)
            projected_b = project_to_wall(b, scene)
            penumbra = max(
                _penumbra(scene, depth, float(a[2])),
                max(texel_x, texel_y) * 1.25,
            )
            margin = edge.width * 0.5 + penumbra + max(texel_x, texel_y) * 2.0
            low_x = max(min_x, float(min(projected_a[0], projected_b[0]) - margin))
            high_x = min(max_x, float(max(projected_a[0], projected_b[0]) + margin))
            low_y = max(min_y, float(min(projected_a[1], projected_b[1]) - margin))
            high_y = min(max_y, float(max(projected_a[1], projected_b[1]) + margin))
            if low_x > high_x or low_y > high_y:
                continue
            ix0 = max(0, int(math.floor((low_x - min_x) / (max_x - min_x) * (resolution - 1))))
            ix1 = min(
                resolution - 1,
                int(math.ceil((high_x - min_x) / (max_x - min_x) * (resolution - 1))),
            )
            iy0 = max(0, int(math.floor((low_y - min_y) / (max_y - min_y) * (resolution - 1))))
            iy1 = min(
                resolution - 1,
                int(math.ceil((high_y - min_y) / (max_y - min_y) * (resolution - 1))),
            )
            if ix0 > ix1 or iy0 > iy1:
                continue
            iy, ix = np.mgrid[iy0 : iy1 + 1, ix0 : ix1 + 1]
            x = min_x + ix.astype(np.float64) / (resolution - 1) * (max_x - min_x)
            y = min_y + iy.astype(np.float64) / (resolution - 1) * (max_y - min_y)
            distance = _distance_to_segment(x, y, projected_a, projected_b)
            local_coverage = _soft_cover(
                distance,
                edge.width * 0.5,
                penumbra,
                scene.shadows.samples,
            )
            coverage[iy0 : iy1 + 1, ix0 : ix1 + 1] = np.maximum(
                coverage[iy0 : iy1 + 1, ix0 : ix1 + 1],
                local_coverage,
            )
    return coverage


def _transition_width(
    values: np.ndarray, axis_positions: np.ndarray
) -> float:
    low, high = 0.08, 0.92
    transition_indices = np.flatnonzero((values > low) & (values < high))
    if transition_indices.size == 0:
        return 0.0
    if transition_indices.size == 1:
        return float(abs(axis_positions[1] - axis_positions[0]))
    start = transition_indices[0]
    end = transition_indices[-1]
    return float(abs(axis_positions[end] - axis_positions[start]))


def _build_profile(
    scene: Scene, edges: tuple[LeadEdge, ...]
) -> tuple[dict[str, Any], str]:
    if not edges:
        return {"probe": None, "depths": []}, hashlib.sha256(b"no-edges").hexdigest()
    probe = max(
        edges,
        key=lambda edge: float(np.linalg.norm((edge.b - edge.a)[:2])),
    )
    max_depth = max(edge.depth for edge in edges)
    level_count = scene.shadows.profile_depth_levels
    if level_count <= 1 or max_depth <= 1e-12:
        depths = [max_depth]
    else:
        depths = [
            max_depth * index / (level_count - 1) for index in range(level_count)
        ]
    profile_points = 161
    span = scene.shadows.profile_span
    trends: list[dict[str, Any]] = []
    digest = hashlib.sha256()
    for depth in depths:
        z = float(probe.a[2] + depth * 0.5)
        a = probe.a.copy()
        b = probe.b.copy()
        a[2] = z
        b[2] = z
        projected_a = project_to_wall(a, scene)
        projected_b = project_to_wall(b, scene)
        direction = projected_b - projected_a
        length = float(np.linalg.norm(direction))
        if length <= 1e-12:
            normal = np.asarray([0.0, 1.0], dtype=np.float64)
        else:
            normal = np.asarray([-direction[1], direction[0]], dtype=np.float64) / length
        midpoint = (projected_a + projected_b) * 0.5
        offsets = np.linspace(-span * 0.5, span * 0.5, profile_points)
        points = midpoint[None, :] + offsets[:, None] * normal[None, :]
        coverage = _edge_coverage_at_points(
            points, probe, scene, depth_override=depth
        )
        width = _transition_width(coverage, offsets)
        trends.append(
            {
                "depth": round(depth, 8),
                "projected_midpoint": [round(float(midpoint[0]), 8), round(float(midpoint[1]), 8)],
                "transition_width": round(width, 8),
                "samples": [
                    {
                        "offset": round(float(offset), 8),
                        "x": round(float(point[0]), 8),
                        "y": round(float(point[1]), 8),
                        "gray": round(float(value), 8),
                    }
                    for offset, point, value in zip(offsets, points, coverage)
                ],
            }
        )
        digest.update(np.asarray(coverage, dtype=np.float32).tobytes())
        digest.update(np.asarray(offsets, dtype=np.float32).tobytes())
    widths = [item["transition_width"] for item in trends]
    monotonic = all(
        widths[index] <= widths[index + 1] + 1e-12
        for index in range(len(widths) - 1)
    )
    profile = {
        "probe_edge": {
            "a": [round(float(value), 8) for value in probe.a],
            "b": [round(float(value), 8) for value in probe.b],
            "width": round(probe.width, 8),
            "depth": round(probe.depth, 8),
        },
        "samples_per_transition": scene.shadows.samples,
        "sample_count": profile_points,
        "span": span,
        "depths": trends,
        "transition_width_monotonic_with_depth": monotonic,
    }
    return profile, digest.hexdigest()


def build_shadow(scene: Scene, edges: tuple[LeadEdge, ...]) -> ShadowResult:
    min_x, max_x, min_y, max_y = _shadow_extent(scene)
    resolution = scene.shadows.resolution
    coverage = _build_coverage_map(scene, edges, min_x, max_x, min_y, max_y)
    projections: list[dict[str, Any]] = []
    for index, edge in enumerate(edges):
        zero_a = project_to_wall(edge.a, scene)
        zero_b = project_to_wall(edge.b, scene)
        depth_a = project_to_wall(edge.a + np.asarray([0.0, 0.0, edge.depth]), scene)
        depth_b = project_to_wall(edge.b + np.asarray([0.0, 0.0, edge.depth]), scene)
        midpoint_zero = (zero_a + zero_b) * 0.5
        midpoint_depth = (depth_a + depth_b) * 0.5
        offset = midpoint_depth - midpoint_zero
        projections.append(
            {
                "edge_index": index,
                "glass_a": [round(float(value), 8) for value in edge.a],
                "glass_b": [round(float(value), 8) for value in edge.b],
                "landing_a": [round(float(zero_a[0]), 8), round(float(zero_a[1]), 8)],
                "landing_b": [round(float(zero_b[0]), 8), round(float(zero_b[1]), 8)],
                "landing_a_at_depth": [
                    round(float(depth_a[0]), 8),
                    round(float(depth_a[1]), 8),
                ],
                "landing_b_at_depth": [
                    round(float(depth_b[0]), 8),
                    round(float(depth_b[1]), 8),
                ],
                "midpoint": [
                    round(float(midpoint_zero[0]), 8),
                    round(float(midpoint_zero[1]), 8),
                ],
                "depth_offset": [
                    round(float(offset[0]), 8),
                    round(float(offset[1]), 8),
                ],
                "lead_width": round(edge.width, 8),
                "lead_depth": round(edge.depth, 8),
            }
        )
    profile, profile_digest = _build_profile(scene, edges)
    digest = hashlib.sha256(
        np.ascontiguousarray(coverage, dtype=np.float32).tobytes()
        + profile_digest.encode("ascii")
    ).hexdigest()
    return ShadowResult(
        coverage=coverage,
        resolution=resolution,
        min_x=min_x,
        max_x=max_x,
        min_y=min_y,
        max_y=max_y,
        projections=tuple(projections),
        profile={
            **profile,
            "profile_digest": profile_digest,
            "coverage_digest": digest,
        },
        digest=digest,
    )
