from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .mathutil import triangle_signed_distance_2d
from .scene import Layer, Scene


@dataclass(frozen=True)
class ProjectionRecord:
    case_id: str
    caster: Layer
    target_kind: str
    target_layer: Layer | None
    receiver_distance: float
    light_distance: float
    penumbra_width: float
    triangles: tuple[dict[str, Any], ...]
    edge_profile: dict[str, Any]

    def as_dict(self, formula: str) -> dict[str, Any]:
        return {
            "case_id": self.case_id,
            "caster_layer_index": self.caster.layer_index,
            "caster_id": self.caster.layer_id,
            "target_kind": self.target_kind,
            "target_layer_index": (
                self.target_layer.layer_index if self.target_layer is not None else None
            ),
            "target_id": (
                self.target_layer.layer_id if self.target_layer is not None else "ground"
            ),
            "receiver_distance": self.receiver_distance,
            "light_distance": self.light_distance,
            "penumbra_width": self.penumbra_width,
            "projection_formula": formula,
            "projected_triangles": list(self.triangles),
            "edge_profile": self.edge_profile,
        }


def penumbra_width(
    scene: Scene,
    receiver_distance: float,
    light_distance: float,
) -> float:
    width = (
        scene.shadow.penumbra_base
        * max(float(receiver_distance), 0.0)
        * (1.0 + max(float(light_distance), 0.0) / scene.shadow.light_distance_scale)
    )
    return float(
        np.clip(width, scene.shadow.min_width, scene.shadow.max_width)
    )


def soft_coverage_from_signed_distance(
    signed_distance: np.ndarray,
    width: np.ndarray | float,
    sample_density: int,
) -> np.ndarray:
    sd = np.asarray(signed_distance, dtype=np.float64)
    w = np.maximum(np.asarray(width, dtype=np.float64), 1.0e-9)
    unit = np.clip(0.5 - sd / w, 0.0, 1.0)
    unit = unit * unit * (3.0 - 2.0 * unit)
    if sample_density > 1:
        unit = np.floor(unit * float(sample_density) + 0.5) / float(sample_density)
    return np.clip(unit, 0.0, 1.0)


def project_along_light(
    point: np.ndarray,
    direction: np.ndarray,
    plane_point: np.ndarray,
    plane_normal: np.ndarray,
) -> tuple[float, np.ndarray]:
    denom = float(np.dot(direction, plane_normal))
    if abs(denom) <= 1.0e-12:
        return float("nan"), np.full(3, np.nan, dtype=np.float64)
    t = float(np.dot(plane_point - point, plane_normal) / denom)
    return t, point + t * direction


def _layer_plane_intersection_distance(
    source: np.ndarray, target: Layer, direction: np.ndarray
) -> float:
    denom = float(np.dot(direction, target.normal))
    if abs(denom) <= 1.0e-12:
        return float("nan")
    return float(np.dot(target.plane_point - source, target.normal) / denom)


def _ground_plane_intersection_distance(
    source: np.ndarray, scene: Scene
) -> float:
    normal = np.array([0.0, 1.0, 0.0], dtype=np.float64)
    denom = float(np.dot(scene.sun.direction, normal))
    if abs(denom) <= 1.0e-12:
        return float("nan")
    point = np.array([0.0, scene.ground.y, 0.0], dtype=np.float64)
    return float(np.dot(point - source, normal) / denom)


def _point_in_layer(layer: Layer, point: np.ndarray, margin: float = 1.0e-9) -> bool:
    relative = point - layer.plane_point
    u = float(np.dot(relative, layer.basis_u))
    v = float(np.dot(relative, layer.basis_v))
    umin, umax, vmin, vmax = layer.uv_bounds
    return (
        umin - margin <= u <= umax + margin
        and vmin - margin <= v <= vmax + margin
    )


def _point_in_ground(scene: Scene, point: np.ndarray, margin: float = 0.0) -> bool:
    xmin, xmax, zmin, zmax = scene.ground.bounds
    return (
        xmin - margin <= point[0] <= xmax + margin
        and zmin - margin <= point[2] <= zmax + margin
        and abs(float(point[1] - scene.ground.y)) <= 1.0e-8
    )


def _nearest_receiving_silk(scene: Scene, caster: Layer) -> tuple[Layer | None, float]:
    candidates: list[tuple[float, Layer]] = []
    for silk in scene.silk_layers:
        distance = _layer_plane_intersection_distance(
            caster.centroid, silk, scene.sun.direction
        )
        if np.isfinite(distance) and distance > 1.0e-9:
            candidates.append((distance, silk))
    if not candidates:
        return None, float("nan")
    distance, silk = min(candidates, key=lambda item: (item[0], item[1].layer_index))
    return silk, distance


def _edge_profile(
    scene: Scene,
    edge_start: np.ndarray,
    edge_end: np.ndarray,
    receiver_normal: np.ndarray,
    width: float,
) -> dict[str, Any]:
    density = max(2, int(scene.shadow.sample_density))
    edge = edge_end - edge_start
    edge_length = float(np.linalg.norm(edge))
    if edge_length <= 1.0e-12:
        edge = np.array([1.0, 0.0, 0.0], dtype=np.float64)
        edge_length = 1.0
    edge_direction = edge / edge_length
    side = np.cross(receiver_normal, edge_direction)
    side_norm = float(np.linalg.norm(side))
    if side_norm <= 1.0e-12:
        side = np.array([0.0, 0.0, 1.0], dtype=np.float64)
    else:
        side = side / side_norm

    midpoint = (edge_start + edge_end) * 0.5
    offsets = np.linspace(-1.25 * width, 1.25 * width, density, dtype=np.float64)
    coverage = soft_coverage_from_signed_distance(
        offsets, width, scene.shadow.sample_density
    )
    brightness = 1.0 - scene.shadow.strength * coverage
    points = midpoint[None, :] + offsets[:, None] * side[None, :]

    samples = []
    for offset, point, cover, lum in zip(offsets, points, coverage, brightness):
        samples.append(
            {
                "offset": float(offset),
                "point": [float(value) for value in point],
                "coverage": float(cover),
                "brightness": float(lum),
            }
        )

    low = float(brightness.min())
    high = float(brightness.max())
    transition_width: float | None = None
    if high - low > 1.0e-9:
        t10 = low + 0.1 * (high - low)
        t90 = low + 0.9 * (high - low)

        def crossing(target: float) -> float | None:
            for index in range(1, len(brightness)):
                a = float(brightness[index - 1])
                b = float(brightness[index])
                if a <= target <= b and b != a:
                    fraction = (target - a) / (b - a)
                    return float(
                        offsets[index - 1] + fraction * (offsets[index] - offsets[index - 1])
                    )
            return None

        o10 = crossing(t10)
        o90 = crossing(t90)
        if o10 is not None and o90 is not None:
            transition_width = abs(o90 - o10)

    return {
        "midpoint": [float(value) for value in midpoint],
        "side_direction": [float(value) for value in side],
        "sample_count": int(density),
        "samples": samples,
        "transition_width_10_90": transition_width,
    }


def build_projection_records(scene: Scene) -> list[ProjectionRecord]:
    records: list[ProjectionRecord] = []
    ground_normal = np.array([0.0, 1.0, 0.0], dtype=np.float64)
    ground_point = np.array([0.0, scene.ground.y, 0.0], dtype=np.float64)

    for caster in scene.mullion_layers:
        light_distance = float(
            np.linalg.norm(caster.centroid - scene.sun.source_position)
        )
        targets: list[tuple[str, Layer | None, np.ndarray, np.ndarray, float]] = []
        silk, silk_distance = _nearest_receiving_silk(scene, caster)
        if silk is not None:
            targets.append(("layer", silk, silk.plane_point, silk.normal, silk_distance))
        ground_distance = _ground_plane_intersection_distance(caster.centroid, scene)
        if np.isfinite(ground_distance) and ground_distance > 1.0e-9:
            targets.append(("ground", None, ground_point, ground_normal, ground_distance))

        for target_kind, target_layer, target_point, target_normal, center_distance in targets:
            width = penumbra_width(scene, center_distance, light_distance)
            projected: list[dict[str, Any]] = []
            profile_edge: tuple[np.ndarray, np.ndarray] | None = None
            profile_edge_length = -1.0
            target_id = target_layer.layer_id if target_layer is not None else "ground"

            for triangle_index, triangle in enumerate(caster.triangles):
                source_vertices = caster.vertices[triangle]
                landing_points: list[list[float]] = []
                valid = True
                in_bounds = True
                for vertex in source_vertices:
                    t, projected_point = project_along_light(
                        vertex, scene.sun.direction, target_point, target_normal
                    )
                    if not np.isfinite(t) or t <= 1.0e-9:
                        valid = False
                        break
                    if target_layer is not None:
                        inside = _point_in_layer(target_layer, projected_point)
                    else:
                        inside = _point_in_ground(scene, projected_point)
                    in_bounds = in_bounds and inside
                    landing_points.append([float(value) for value in projected_point])
                if not valid or len(landing_points) != 3:
                    projected.append(
                        {
                            "triangle_index": triangle_index,
                            "valid": False,
                            "all_vertices_in_bounds": False,
                            "source_centroid": [
                                float(value) for value in np.mean(source_vertices, axis=0)
                            ],
                            "landing_points": [],
                        }
                    )
                    continue

                source_centroid = np.mean(source_vertices, axis=0)
                _, landing_centroid = project_along_light(
                    source_centroid, scene.sun.direction, target_point, target_normal
                )
                projected.append(
                    {
                        "triangle_index": triangle_index,
                        "valid": True,
                        "all_vertices_in_bounds": bool(in_bounds),
                        "source_centroid": [
                            float(value) for value in source_centroid
                        ],
                        "landing_centroid": [
                            float(value) for value in landing_centroid
                        ],
                        "landing_points": landing_points,
                    }
                )

                if in_bounds:
                    points = np.asarray(landing_points, dtype=np.float64)
                    for a, b in ((0, 1), (1, 2), (2, 0)):
                        length = float(np.linalg.norm(points[b] - points[a]))
                        if length > profile_edge_length:
                            profile_edge_length = length
                            profile_edge = (points[a], points[b])

            if profile_edge is None:
                fallback = np.mean(caster.vertices, axis=0)
                _, fallback_landing = project_along_light(
                    fallback, scene.sun.direction, target_point, target_normal
                )
                profile_edge = (
                    fallback_landing - np.array([0.05, 0.0, 0.0]),
                    fallback_landing + np.array([0.05, 0.0, 0.0]),
                )
            edge_profile = _edge_profile(
                scene, profile_edge[0], profile_edge[1], target_normal, width
            )
            in_bounds_count = sum(
                1 for item in projected if item.get("all_vertices_in_bounds")
            )
            records.append(
                ProjectionRecord(
                    case_id=f"{caster.layer_id}->{target_id}",
                    caster=caster,
                    target_kind=target_kind,
                    target_layer=target_layer,
                    receiver_distance=float(center_distance),
                    light_distance=light_distance,
                    penumbra_width=float(width),
                    triangles=tuple(projected),
                    edge_profile=edge_profile,
                )
            )
    return records


def projection_report(scene: Scene) -> dict[str, Any]:
    records = build_projection_records(scene)
    formula = "q = p + t*d, t = dot(plane_point - p, plane_normal) / dot(d, plane_normal)"
    cases = [record.as_dict(formula) for record in records]
    sample_count = sum(
        int(record.edge_profile["sample_count"]) for record in records
    )
    projected_triangles = sum(len(record.triangles) for record in records)
    in_bounds_triangles = sum(
        1
        for record in records
        for triangle in record.triangles
        if triangle.get("all_vertices_in_bounds")
    )

    mean_light_distance = float(
        np.mean([record.light_distance for record in records]) if records else 0.0
    )
    distance_cases = []
    for distance in (0.03, 0.06, 0.12):
        width = penumbra_width(scene, distance, mean_light_distance)
        distance_cases.append(
            {
                "receiver_distance": distance,
                "light_distance": mean_light_distance,
                "penumbra_width": width,
            }
        )
    light_cases = []
    for light_distance in (6.0, 12.0, 24.0):
        width = penumbra_width(scene, 0.06, light_distance)
        light_cases.append(
            {
                "receiver_distance": 0.06,
                "light_distance": light_distance,
                "penumbra_width": width,
            }
        )

    return {
        "format_version": 1,
        "scene_digest": scene.digest,
        "projection_formula": formula,
        "note": "Projected points outside the finite receiver bounds are reported but ignored by shading.",
        "sample_density": scene.shadow.sample_density,
        "projection_case_count": len(records),
        "projected_triangle_count": projected_triangles,
        "in_bounds_triangle_count": in_bounds_triangles,
        "edge_profile_sample_points": sample_count,
        "cases": cases,
        "penumbra_trend": {
            "receiver_distance_cases": distance_cases,
            "light_distance_cases": light_cases,
        },
    }


def _mesh_signed_distance_2d(points2d: np.ndarray, mesh: Layer) -> np.ndarray:
    best = np.full(points2d.shape[0], np.inf, dtype=np.float64)
    relative = mesh.vertices - mesh.plane_point
    uv = np.stack(
        (relative @ mesh.basis_u, relative @ mesh.basis_v),
        axis=1,
    )
    for triangle in mesh.triangles:
        distance = triangle_signed_distance_2d(
            points2d,
            uv[int(triangle[0])],
            uv[int(triangle[1])],
            uv[int(triangle[2])],
        )
        best = np.minimum(best, distance)
    return best


def _ground_edge_fade(scene: Scene, points: np.ndarray) -> np.ndarray:
    xmin, xmax, zmin, zmax = scene.ground.bounds
    margin = max(0.03, 0.06 * min(xmax - xmin, zmax - zmin))
    distance = np.minimum.reduce(
        (
            points[:, 0] - xmin,
            xmax - points[:, 0],
            points[:, 2] - zmin,
            zmax - points[:, 2],
        )
    )
    unit = np.clip(distance / margin, 0.0, 1.0)
    return unit * unit * (3.0 - 2.0 * unit)


def shadow_coverage_for_caster(
    scene: Scene,
    receiver_points: np.ndarray,
    caster: Layer,
    ground: bool = False,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    points = np.asarray(receiver_points, dtype=np.float64).reshape(-1, 3)
    count = points.shape[0]
    if count == 0:
        return (
            np.empty(0, dtype=np.float64),
            np.empty(0, dtype=np.bool_),
            np.empty(0, dtype=np.float64),
        )

    toward_sun = -scene.sun.direction
    denom = float(np.dot(toward_sun, caster.normal))
    if abs(denom) <= 1.0e-12:
        return np.zeros(count), np.zeros(count, dtype=np.bool_), np.zeros(count)
    t = ((caster.plane_point[None, :] - points) @ caster.normal) / denom
    valid = np.isfinite(t) & (t >= 0.0)
    projected = points + t[:, None] * toward_sun[None, :]
    relative = projected - caster.plane_point[None, :]
    points2d = np.stack(
        (relative @ caster.basis_u, relative @ caster.basis_v),
        axis=1,
    )
    signed_distance = _mesh_signed_distance_2d(points2d, caster)

    light_distance = float(
        np.linalg.norm(caster.centroid - scene.sun.source_position)
    )
    widths = (
        scene.shadow.penumbra_base
        * np.maximum(t, 0.0)
        * (1.0 + light_distance / scene.shadow.light_distance_scale)
    )
    widths = np.clip(widths, scene.shadow.min_width, scene.shadow.max_width)
    coverage = soft_coverage_from_signed_distance(
        signed_distance, widths, scene.shadow.sample_density
    )
    if ground:
        xmin, xmax, zmin, zmax = scene.ground.bounds
        inside_receiver = (
            (points[:, 0] >= xmin)
            & (points[:, 0] <= xmax)
            & (points[:, 2] >= zmin)
            & (points[:, 2] <= zmax)
        )
        coverage = np.where(inside_receiver, coverage, 0.0)
        coverage = coverage * _ground_edge_fade(scene, points)
    coverage = np.where(valid, coverage, 0.0)
    return coverage, valid, widths


def combined_shadow(
    scene: Scene,
    receiver_points: np.ndarray,
    ground: bool = False,
) -> tuple[np.ndarray, int]:
    points = np.asarray(receiver_points, dtype=np.float64).reshape(-1, 3)
    result = np.zeros(points.shape[0], dtype=np.float64)
    if points.shape[0] == 0:
        return result, 0
    transmission = np.ones(points.shape[0], dtype=np.float64)
    evaluations = 0
    for caster in scene.mullion_layers:
        coverage, _, _ = shadow_coverage_for_caster(scene, points, caster, ground=ground)
        transmission *= 1.0 - scene.shadow.strength * coverage
        evaluations += points.shape[0]
    result = np.clip(1.0 - transmission, 0.0, 1.0)
    return result, evaluations
