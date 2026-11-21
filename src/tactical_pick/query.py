"""Screen-to-ray picking against one complete snapshot."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Sequence

from .camera import Camera
from .errors import fail
from .math3d import (
    EPS,
    Ray,
    Vec3,
    add,
    length,
    mul,
    normalize,
    ray_aabb,
    ray_capsule,
    ray_triangle,
    validate_ray,
)
from .scene import CoverTriangle, Unit
from .snapshot import SceneManager, SceneSnapshot, SnapshotHandle


@dataclass(frozen=True)
class PickRequest:
    screen: tuple[float, float]
    camera: Camera
    requested_version: int | None = None


@dataclass(frozen=True)
class Hit:
    entity_kind: str
    entity_id: int | None
    entity: str
    point: Vec3
    normal: Vec3
    distance: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "entity_kind": self.entity_kind,
            "entity_id": self.entity_id,
            "entity": self.entity,
            "point": list(self.point),
            "normal": list(self.normal),
            "distance": self.distance,
        }


@dataclass(frozen=True)
class PickResult:
    screen: tuple[float, float]
    ray: Ray
    scene_version: int
    scene_hash: str
    hit: Hit | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "screen": list(self.screen),
            "ray": {"origin": list(self.ray.origin), "direction": list(self.ray.direction)},
            "scene_version": self.scene_version,
            "scene_hash": self.scene_hash,
            "hit": None if self.hit is None else self.hit.to_dict(),
        }


def _priority(hit: Hit) -> tuple[float, int, int]:
    kind_priority = {"unit": 0, "cover": 1, "ground": 2}.get(hit.entity_kind, 3)
    entity_id = -1 if hit.entity_id is None else hit.entity_id
    return (hit.distance, kind_priority, entity_id)


def _unit_hit(ray: Ray, unit: Unit) -> tuple[float, Vec3] | None:
    if unit.kind == "aabb":
        return ray_aabb(ray.origin, ray.direction, unit.aabb_min, unit.aabb_max)
    return ray_capsule(ray.origin, ray.direction, unit.capsule_a, unit.capsule_b, unit.radius)


def _cover_hit(ray: Ray, triangle: CoverTriangle) -> tuple[float, Vec3] | None:
    return ray_triangle(ray.origin, ray.direction, triangle.a, triangle.b, triangle.c)


def _ground_hit(ray: Ray, ground_y: float) -> tuple[float, Vec3] | None:
    if abs(ray.direction[1]) <= EPS:
        return None
    distance = (ground_y - ray.origin[1]) / ray.direction[1]
    if distance < -EPS:
        return None
    return (max(0.0, distance), (0.0, 1.0, 0.0))


def _hit_from_distance(ray: Ray, kind: str, entity_id: int | None, distance: float, normal: Vec3) -> Hit:
    entity = "ground" if kind == "ground" else f"{kind}:{entity_id}"
    point = add(ray.origin, mul(ray.direction, distance))
    return Hit(kind, entity_id, entity, point, normalize(normal), distance)


def intersect_snapshot(snapshot: SceneSnapshot, ray: Ray) -> Hit | None:
    validate_ray(ray)
    best: Hit | None = None

    for index in snapshot.unit_bvh.candidate_indices(ray):
        unit = snapshot.scene.units[index]
        result = _unit_hit(ray, unit)
        if result is None:
            continue
        distance, normal = result
        hit = _hit_from_distance(ray, "unit", unit.unit_id, distance, normal)
        if best is None or _priority(hit) < _priority(best):
            best = hit

    for index in snapshot.cover_bvh.candidate_indices(ray):
        triangle = snapshot.scene.cover_triangles[index]
        result = _cover_hit(ray, triangle)
        if result is None:
            continue
        distance, normal = result
        hit = _hit_from_distance(ray, "cover", triangle.entity_id, distance, normal)
        if best is None or _priority(hit) < _priority(best):
            best = hit

    ground = _ground_hit(ray, snapshot.scene.ground_y)
    if ground is not None:
        distance, normal = ground
        hit = _hit_from_distance(ray, "ground", None, distance, normal)
        if best is None or _priority(hit) < _priority(best):
            best = hit

    if best is not None and (not math.isfinite(best.distance) or length(best.normal) <= EPS):
        fail("E_INTERNAL", "query produced a non-finite hit", "query.hit")
    return best


class Picker:
    def __init__(self, manager: SceneManager) -> None:
        self.manager = manager

    def pick(self, request: PickRequest, handle: SnapshotHandle | None = None) -> PickResult:
        # All request validation happens before any scene result is materialized.
        ray = request.camera.screen_to_ray(request.screen)
        owned = handle is None
        active = self.manager.acquire(request.requested_version) if handle is None else handle
        try:
            snapshot = active.snapshot()
            if request.requested_version is not None and snapshot.version != request.requested_version:
                fail(
                    "E_VERSION_STALE",
                    "query request and snapshot handle refer to different versions",
                    "snapshot.requested_version",
                    first_conflict={
                        "requested_version": request.requested_version,
                        "handle_version": snapshot.version,
                    },
                )
            hit = intersect_snapshot(snapshot, ray)
            return PickResult(
                screen=(float(request.screen[0]), float(request.screen[1])),
                ray=ray,
                scene_version=snapshot.version,
                scene_hash=snapshot.content_hash,
                hit=hit,
            )
        finally:
            if owned:
                active.release()

    def pick_ray(self, ray: Ray, requested_version: int | None = None) -> PickResult:
        validate_ray(ray)
        ray = Ray(ray.origin, normalize(ray.direction))
        active = self.manager.acquire(requested_version)
        try:
            snapshot = active.snapshot()
            hit = intersect_snapshot(snapshot, ray)
            return PickResult(
                screen=(-1.0, -1.0),
                ray=ray,
                scene_version=snapshot.version,
                scene_hash=snapshot.content_hash,
                hit=hit,
            )
        finally:
            active.release()

