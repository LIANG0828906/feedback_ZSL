"""Fish quad projection, soft-shadow profiles and deterministic shadow maps."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .scene import Component, Fish, Light, Receiver, Scene


def smoothstep(edge0: float, edge1: float, value: np.ndarray | float) -> np.ndarray:
    if edge1 <= edge0:
        return (np.asarray(value) >= edge1).astype(np.float64)
    t = np.clip((np.asarray(value) - edge0) / (edge1 - edge0), 0.0, 1.0)
    return t * t * (3.0 - 2.0 * t)


def penumbra_width(
    scene: Scene,
    light: Light,
    fish_centroid: np.ndarray,
    target_centroid: np.ndarray,
) -> float:
    """Deterministic stylised area-light penumbra width in scene units.

    The width is monotonic in both requested distances. It is a scene-authored
    artistic response, not a quantum of a Monte-Carlo estimator, so repeated
    runs and camera orbits cannot change the transition.
    """

    d_fish_target = float(np.linalg.norm(fish_centroid - target_centroid))
    d_light_fish = float(np.linalg.norm(light.position - fish_centroid))
    scale = max(scene.render.shadow_softness_scale, 1e-4)
    density_factor = 1.0 + 0.35 / math.sqrt(max(scene.render.shadow_samples, 1))
    return scene.render.shadow_softness_base * density_factor * (
        (1.0 + d_fish_target / scale) * (1.0 + d_light_fish / scale)
    )


def project_to_plane(light_position: np.ndarray, points: np.ndarray, plane_z: float) -> np.ndarray | None:
    denominator = points[:, 2] - light_position[2]
    if np.any(np.abs(denominator) <= 1e-9):
        return None
    t = (plane_z - light_position[2]) / denominator
    if np.any(t <= 1e-6):
        return None
    return light_position[None, :] + t[:, None] * (points - light_position[None, :])


def project_to_cylinder(
    light_position: np.ndarray,
    points: np.ndarray,
    radius: float,
) -> np.ndarray | None:
    origin = light_position[:2]
    result = []
    for point in points:
        direction = point[:2] - origin
        a = float(np.dot(direction, direction))
        if a <= 1e-12:
            return None
        b = 2.0 * float(np.dot(origin, direction))
        c = float(np.dot(origin, origin)) - radius * radius
        discriminant = b * b - 4.0 * a * c
        if discriminant < -1e-10:
            return None
        discriminant = max(discriminant, 0.0)
        roots = [(-b - math.sqrt(discriminant)) / (2.0 * a), (-b + math.sqrt(discriminant)) / (2.0 * a)]
        positive = [root for root in roots if root > 1e-8]
        if not positive:
            return None
        t = min(positive)
        result.append(light_position + t * (point - light_position))
    return np.asarray(result, dtype=np.float64)


def _angles_unwrapped(points: np.ndarray, center_angle: float) -> np.ndarray:
    angles = np.arctan2(points[:, 1], points[:, 0])
    return center_angle + np.angle(np.exp(1j * (angles - center_angle)))


def _receiver_contains(points: np.ndarray, receiver: Receiver, margin: float = 0.0) -> bool:
    radii = np.hypot(points[:, 0], points[:, 1])
    if np.any(radii < receiver.inner_radius - margin) or np.any(radii > receiver.outer_radius + margin):
        return False
    if receiver.angle_end_deg - receiver.angle_start_deg >= 359.999:
        return True
    angle = np.degrees(np.arctan2(points[:, 1], points[:, 0]))
    start = receiver.angle_start_deg
    end = receiver.angle_end_deg
    # The sample receiver is smaller than a full turn. Use direct degree checks.
    return bool(np.all(angle >= start - margin) and np.all(angle <= end + margin))


def receiver_ring_mask(scene: Scene, points: np.ndarray) -> np.ndarray:
    receiver = scene.receiver
    radii = np.hypot(points[:, 0], points[:, 1])
    soft = max(0.008, (receiver.outer_radius - receiver.inner_radius) * 0.018)
    radial = smoothstep(receiver.inner_radius - soft, receiver.inner_radius + soft, radii)
    radial *= 1.0 - smoothstep(receiver.outer_radius - soft, receiver.outer_radius + soft, radii)
    if receiver.angle_end_deg - receiver.angle_start_deg >= 359.999:
        return radial
    angles = np.degrees(np.arctan2(points[:, 1], points[:, 0]))
    angular = smoothstep(receiver.angle_start_deg - 0.5, receiver.angle_start_deg + 0.5, angles)
    angular *= 1.0 - smoothstep(receiver.angle_end_deg - 0.5, receiver.angle_end_deg + 0.5, angles)
    return radial * angular


def _component_z_range(component: Component) -> tuple[float, float]:
    return float(component.vertices[:, 2].min()), float(component.vertices[:, 2].max())


def _component_angle_range(component: Component) -> tuple[float, float]:
    angles = np.degrees(np.arctan2(component.vertices[:, 1], component.vertices[:, 0]))
    return float(angles.min()), float(angles.max())


def _water_contains(points: np.ndarray, component: Component, margin: float = 0.3) -> bool:
    z0, z1 = _component_z_range(component)
    if np.any(points[:, 2] < z0 - margin) or np.any(points[:, 2] > z1 + margin):
        return False
    a0, a1 = _component_angle_range(component)
    angles = np.degrees(np.arctan2(points[:, 1], points[:, 0]))
    return bool(np.all(angles >= a0 - margin) and np.all(angles <= a1 + margin))


@dataclass
class ProjectedPolygon:
    fish_id: str
    light_id: str
    target: str
    points: np.ndarray
    points2d: np.ndarray
    valid: bool
    centroid: np.ndarray
    edge_width: float
    component_id: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": "valid" if self.valid else "outside",
            "points": [[round(float(v), 8) for v in point] for point in self.points],
            "centroid": [round(float(v), 8) for v in self.centroid],
            "edge_width": round(float(self.edge_width), 8),
            **({"water_component_id": self.component_id} if self.component_id else {}),
        }


def _polygon_signed_distance(points2d: np.ndarray, queries: np.ndarray) -> np.ndarray:
    """Signed distance for a convex polygon, positive inside."""

    if len(points2d) < 3:
        return np.full(len(queries), -np.inf)
    area = 0.0
    for i in range(len(points2d)):
        j = (i + 1) % len(points2d)
        area += points2d[i, 0] * points2d[j, 1] - points2d[j, 0] * points2d[i, 1]
    sign = 1.0 if area >= 0.0 else -1.0
    signed = np.full(len(queries), np.inf)
    for i in range(len(points2d)):
        a = points2d[i]
        b = points2d[(i + 1) % len(points2d)]
        edge = b - a
        length = float(np.linalg.norm(edge))
        if length <= 1e-12:
            continue
        normal = sign * np.asarray([-edge[1], edge[0]]) / length
        distance = (queries - a[None, :]) @ normal
        signed = np.minimum(signed, distance)
    return signed


def polygon_coverage(
    polygon: ProjectedPolygon,
    queries2d: np.ndarray,
    strength: float,
) -> np.ndarray:
    if not polygon.valid or len(queries2d) == 0:
        return np.zeros(len(queries2d), dtype=np.float64)
    signed = _polygon_signed_distance(polygon.points2d, queries2d)
    half_width = max(polygon.edge_width * 0.5, 1e-6)
    return float(strength) * smoothstep(-half_width, half_width, signed)


def build_projection_geometry(scene: Scene) -> dict[str, Any]:
    scene.render  # keeps the mypy-friendly explicit use in one place
    receiver_polygons: list[ProjectedPolygon] = []
    water_polygons: list[ProjectedPolygon] = []
    report_lights: list[dict[str, Any]] = []

    for light in scene.lights:
        fish_entries = []
        for fish in scene.fish:
            fish_centroid = fish.vertices.mean(axis=0)
            receiver_points = project_to_plane(
                light.position, fish.vertices, scene.receiver.plane_z
            )
            receiver_polygon: ProjectedPolygon
            if receiver_points is None:
                receiver_polygon = ProjectedPolygon(
                    fish_id=fish.fish_id,
                    light_id=light.light_id,
                    target="receiver",
                    points=np.zeros((4, 3)),
                    points2d=np.zeros((4, 2)),
                    valid=False,
                    centroid=np.zeros(3),
                    edge_width=0.0,
                    component_id=scene.receiver.component_id,
                )
            else:
                receiver_valid = _receiver_contains(receiver_points, scene.receiver, margin=1e-6)
                receiver_centroid = receiver_points.mean(axis=0)
                width = penumbra_width(scene, light, fish_centroid, receiver_centroid)
                receiver_polygon = ProjectedPolygon(
                    fish_id=fish.fish_id,
                    light_id=light.light_id,
                    target="receiver",
                    points=receiver_points,
                    points2d=receiver_points[:, :2],
                    valid=receiver_valid,
                    centroid=receiver_centroid,
                    edge_width=width,
                    component_id=scene.receiver.component_id,
                )
                receiver_polygons.append(receiver_polygon)

            target_water = next(
                (component for component in scene.water if component.component_id == fish.water_component_id),
                scene.water[0],
            )
            target_radius = (
                target_water.projection_radius
                if target_water.projection_radius is not None
                else float(np.hypot(target_water.vertices[:, 0], target_water.vertices[:, 1]).max())
            )
            water_points = project_to_cylinder(light.position, fish.vertices, target_radius)
            water_polygon: ProjectedPolygon
            if water_points is None:
                water_polygon = ProjectedPolygon(
                    fish_id=fish.fish_id,
                    light_id=light.light_id,
                    target="water",
                    points=np.zeros((4, 3)),
                    points2d=np.zeros((4, 2)),
                    valid=False,
                    centroid=np.zeros(3),
                    edge_width=0.0,
                    component_id=target_water.component_id,
                )
            else:
                water_valid = _water_contains(water_points, target_water)
                water_centroid = water_points.mean(axis=0)
                center_angle = float(math.atan2(water_centroid[1], water_centroid[0]))
                unwrapped = _angles_unwrapped(water_points, center_angle)
                points2d = np.column_stack([unwrapped * target_radius, water_points[:, 2]])
                width = penumbra_width(scene, light, fish_centroid, water_centroid)
                water_polygon = ProjectedPolygon(
                    fish_id=fish.fish_id,
                    light_id=light.light_id,
                    target="water",
                    points=water_points,
                    points2d=points2d,
                    valid=water_valid,
                    centroid=water_centroid,
                    edge_width=width,
                    component_id=target_water.component_id,
                )
                water_polygons.append(water_polygon)

            profile = build_profile_samples(
                scene,
                light,
                fish,
                receiver_polygon,
                strength=scene.render.shadow_strength,
            )
            fish_entries.append(
                {
                    "fish_id": fish.fish_id,
                    "fish_centroid": [round(float(v), 8) for v in fish_centroid],
                    "receiver": receiver_polygon.to_dict(),
                    "water": water_polygon.to_dict(),
                    "profile_samples": profile,
                    "distances": {
                        "fish_to_receiver": round(
                            float(np.linalg.norm(fish_centroid - receiver_polygon.centroid)), 8
                        ),
                        "light_to_fish": round(float(np.linalg.norm(light.position - fish_centroid)), 8),
                    },
                }
            )
        report_lights.append({"light_id": light.light_id, "fish": fish_entries})

    return {
        "receiver_polygons": receiver_polygons,
        "water_polygons": water_polygons,
        "lights": report_lights,
    }


def build_profile_samples(
    scene: Scene,
    light: Light,
    fish: Fish,
    polygon: ProjectedPolygon,
    strength: float,
) -> list[dict[str, Any]]:
    if not polygon.valid:
        return []
    points = polygon.points2d
    first_edge = points[1] - points[0]
    length = float(np.linalg.norm(first_edge))
    if length <= 1e-12:
        return []
    edge_mid = (points[0] + points[1]) * 0.5
    normal = np.asarray([first_edge[1], -first_edge[0]]) / length
    center = points.mean(axis=0)
    if np.dot(center - edge_mid, normal) < 0.0:
        normal = -normal
    width = max(polygon.edge_width, 1e-6)
    sample_count = max(5, min(2 * (scene.render.shadow_samples // 2) + 1, 17))
    offsets = np.linspace(-1.2 * width, 1.2 * width, sample_count)
    probe = edge_mid[None, :] + offsets[:, None] * normal[None, :]
    coverage = polygon_coverage(polygon, probe, strength)
    brightness = np.clip(1.0 - coverage, 0.0, 1.0)
    return [
        {
            "offset": round(float(offsets[index]), 8),
            "position": [round(float(probe[index, 0]), 8), round(float(probe[index, 1]), 8)],
            "brightness": round(float(brightness[index]), 8),
        }
        for index in range(len(offsets))
    ]


def receiver_shadow_factors(
    scene: Scene,
    projection_geometry: dict[str, Any],
    points: np.ndarray,
) -> tuple[np.ndarray, dict[str, np.ndarray]]:
    """Return per-light visibility factors at points on the pebble receiver."""

    count = len(points)
    factors: dict[str, np.ndarray] = {}
    queries = points[:, :2]
    ring_mask = receiver_ring_mask(scene, points)
    for light in scene.lights:
        blocked = np.zeros(count, dtype=np.float64)
        for polygon in projection_geometry["receiver_polygons"]:
            if polygon.light_id != light.light_id:
                continue
            blocked = np.maximum(
                blocked,
                polygon_coverage(polygon, queries, scene.render.shadow_strength),
            )
        factors[light.light_id] = np.clip((1.0 - blocked) * ring_mask, 0.0, 1.0)
    return ring_mask, factors


def water_shadow_factors(
    scene: Scene,
    component: Component,
    projection_geometry: dict[str, Any],
    points: np.ndarray,
) -> dict[str, np.ndarray]:
    factors: dict[str, np.ndarray] = {}
    if len(points) == 0:
        return {light.light_id: np.ones(0) for light in scene.lights}
    center_angle = float(np.arctan2(points[:, 1], points[:, 0]).mean())
    radii = np.hypot(points[:, 0], points[:, 1])
    radius = float(np.mean(radii))
    unwrapped = _angles_unwrapped(points, center_angle)
    queries = np.column_stack([unwrapped * radius, points[:, 2]])
    for light in scene.lights:
        blocked = np.zeros(len(points), dtype=np.float64)
        for polygon in projection_geometry["water_polygons"]:
            if polygon.light_id != light.light_id or polygon.component_id != component.component_id:
                continue
            blocked = np.maximum(
                blocked,
                polygon_coverage(polygon, queries, scene.render.shadow_strength),
            )
        factors[light.light_id] = np.clip(1.0 - blocked, 0.0, 1.0)
    return factors


def build_shadow_map(scene: Scene, light: Light, projection_geometry: dict[str, Any]) -> np.ndarray:
    """Top-down receiver grayscale map for one light.

    The declared ``shadow_samples`` density controls deterministic subpixel
    supersampling. The same scene therefore always produces identical bytes.
    """

    resolution = scene.render.shadow_map_resolution
    extent = scene.receiver.outer_radius * 1.04
    axis = np.linspace(-extent, extent, resolution, dtype=np.float64)
    xx, yy = np.meshgrid(axis, axis)
    aa_axis = max(1, int(math.sqrt(scene.render.shadow_samples)))
    offsets = (np.arange(aa_axis, dtype=np.float64) + 0.5) / aa_axis - 0.5
    pixel_step = (2.0 * extent) / max(resolution - 1, 1)
    accumulated = np.zeros((resolution, resolution), dtype=np.float64)
    for oy in offsets:
        for ox in offsets:
            px = xx + ox * pixel_step
            py = yy + oy * pixel_step
            points = np.column_stack([px.ravel(), py.ravel(), np.full(px.size, scene.receiver.plane_z)])
            mask = receiver_ring_mask(scene, points).reshape(resolution, resolution)
            blocked = np.zeros(points.shape[0], dtype=np.float64)
            for polygon in projection_geometry["receiver_polygons"]:
                if polygon.light_id != light.light_id:
                    continue
                blocked = np.maximum(
                    blocked,
                    polygon_coverage(
                        polygon,
                        points[:, :2],
                        scene.render.shadow_strength,
                    ),
                )
            visible = np.clip((1.0 - blocked) * mask.reshape(-1), 0.0, 1.0)
            accumulated += visible.reshape(resolution, resolution)
    count = max(1, aa_axis * aa_axis)
    grayscale = accumulated / count
    # Keep the ring boundary smooth rather than painting a hard black exterior.
    return np.clip(np.rint(grayscale * 255.0), 0, 255).astype(np.uint8)


def projection_report(projection_geometry: dict[str, Any]) -> dict[str, Any]:
    return {"lights": projection_geometry["lights"]}

