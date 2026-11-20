from __future__ import annotations

import math
from typing import Any

from .errors import E_CONFIG_INVALID, fail
from .model import Point, Scene, Wall


SPEED_OF_SOUND_MPS = 343.0


def distance(a: Point, b: Point) -> float:
    return math.hypot(a.x - b.x, a.y - b.y)


def _mirror(point: Point, wall: Wall) -> tuple[float, float]:
    x1, y1, x2, y2 = wall.x1, wall.y1, wall.x2, wall.y2
    dx, dy = x2 - x1, y2 - y1
    length_sq = dx * dx + dy * dy
    if length_sq <= 1e-12:
        fail(E_CONFIG_INVALID, f"wall {wall.id} has zero length")
    projection = ((point.x - x1) * dx + (point.y - y1) * dy) / length_sq
    foot_x = x1 + projection * dx
    foot_y = y1 + projection * dy
    return 2.0 * foot_x - point.x, 2.0 * foot_y - point.y


def _segment_intersection(
    origin: tuple[float, float],
    target: tuple[float, float],
    wall: Wall,
) -> tuple[float, float] | None:
    ax, ay = origin
    bx, by = target
    cx, cy = wall.x1, wall.y1
    dx, dy = wall.x2, wall.y2
    denominator = (bx - ax) * (dy - cy) - (by - ay) * (dx - cx)
    if abs(denominator) <= 1e-12:
        return None
    t = ((cx - ax) * (dy - cy) - (cy - ay) * (dx - cx)) / denominator
    u = ((cx - ax) * (by - ay) - (cy - ay) * (bx - ax)) / denominator
    if -1e-9 <= t <= 1.0 + 1e-9 and -1e-9 <= u <= 1.0 + 1e-9:
        return ax + t * (bx - ax), ay + t * (by - ay)
    return None


def reflection_paths(scene: Scene, listener_id: str) -> list[dict[str, Any]]:
    listener = scene.listener(listener_id)
    results: list[dict[str, Any]] = []
    direct_distance = distance(scene.source, listener)
    for wall in scene.walls:
        mirrored = _mirror(scene.source, wall)
        intersection = _segment_intersection(mirrored, (listener.x, listener.y), wall)
        if intersection is None:
            continue
        image_point = Point("image", mirrored[0], mirrored[1], scene.source.z)
        path_length = distance(image_point, listener)
        hit = Point("hit", intersection[0], intersection[1], scene.source.z)
        results.append(
            {
                "wall_id": wall.id,
                "material": wall.material,
                "path_length_m": path_length,
                "excess_path_m": max(0.0, path_length - direct_distance),
                "delay_ms": 1000.0 * path_length / SPEED_OF_SOUND_MPS,
                "excess_delay_ms": 1000.0
                * max(0.0, path_length - direct_distance)
                / SPEED_OF_SOUND_MPS,
                "points": [
                    [scene.source.x, scene.source.y],
                    [hit.x, hit.y],
                    [listener.x, listener.y],
                ],
            }
        )
    results.sort(key=lambda item: (item["delay_ms"], item["wall_id"]))
    return results


def listeners_for_wall(scene: Scene, wall_id: str) -> list[str]:
    affected = []
    for listener in scene.listeners:
        if any(
            path["wall_id"] == wall_id
            for path in reflection_paths(scene, listener.id)
        ):
            affected.append(listener.id)
    return affected


def material_signature(
    scene: Scene,
    materials: dict[str, Any],
    centers: list[float],
) -> dict[str, Any]:
    signature: dict[str, Any] = {}
    for wall in scene.walls:
        material = materials.get(wall.material, {})
        absorption = (
            material.get("absorption", []) if isinstance(material, dict) else material
        )
        values = [float(value) for value in absorption]
        if len(values) < len(centers):
            values = values + [values[-1] if values else 0.1] * (
                len(centers) - len(values)
            )
        signature[wall.id] = {
            "material": wall.material,
            "absorption": values[: len(centers)],
        }
    return {
        "walls": signature,
        "center_frequencies": list(centers),
    }


def event_wall_map(
    scene: Scene,
    listener_id: str,
    event_times_ms: list[float],
    tolerance_ms: float = 18.0,
) -> dict[int, str]:
    paths = reflection_paths(scene, listener_id)
    if not paths:
        return {}
    mapping: dict[int, str] = {}
    for event_index, event_time in enumerate(event_times_ms):
        best = min(
            paths,
            key=lambda item: (
                abs(float(item["delay_ms"]) - event_time),
                item["wall_id"],
            ),
        )
        if abs(float(best["delay_ms"]) - event_time) <= tolerance_ms:
            mapping[event_index] = str(best["wall_id"])
    return mapping

