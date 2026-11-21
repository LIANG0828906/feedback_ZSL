"""Screen ray picking across units, cover triangles and the ground."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Sequence

from .camera import Camera, Ray
from .errors import E_VERSION_STALE, W_RAY_GRAZING, fail
from .geometry import (
    Vec3,
    near_grazing,
    ray_aabb_intersection,
    ray_capsule_intersection,
    ray_plane_z_intersection,
    ray_triangle_intersection,
)
from .versioning import RegionSnapshot, SnapshotHandle, VersionStore


@dataclass(frozen=True)
class Hit:
    kind: str
    entity_id: int | None
    point: Vec3 | None
    normal: Vec3 | None
    distance: float | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "entity_id": self.entity_id,
            "point": list(self.point) if self.point is not None else None,
            "normal": list(self.normal) if self.normal is not None else None,
            "distance": self.distance,
        }


@dataclass(frozen=True)
class PickDiagnostic:
    code: str
    message: str
    field: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "field": self.field,
        }


@dataclass(frozen=True)
class PickResult:
    screen: tuple[float, float]
    ray: Ray
    version: int
    content_hash: str
    hit: Hit
    diagnostics: tuple[PickDiagnostic, ...] = field(default_factory=tuple)

    def to_dict(self) -> dict[str, Any]:
        return {
            "screen": list(self.screen),
            "ray": self.ray.to_dict(),
            "region_version": self.version,
            "region_hash": self.content_hash,
            "hit": self.hit.to_dict(),
            "diagnostics": [item.to_dict() for item in self.diagnostics],
        }


def _pick_on_snapshot(
    snapshot: RegionSnapshot,
    camera: Camera,
    screen: Sequence[float],
) -> PickResult:
    ray = camera.screen_to_ray(screen)
    scene = snapshot.scene
    candidates: list[tuple[float, int, int, str, Vec3, Vec3]] = []
    active_units = scene.active_units()

    def point_at(t: float) -> Vec3:
        return (
            ray.origin[0] + ray.direction[0] * t,
            ray.origin[1] + ray.direction[1] * t,
            ray.origin[2] + ray.direction[2] * t,
        )

    for candidate_index in snapshot.unit_bvh.candidates(ray.origin, ray.direction):
        unit = active_units[candidate_index]
        if unit.bounds_kind == "capsule":
            assert unit.capsule_a is not None and unit.capsule_b is not None
            hit = ray_capsule_intersection(
                ray.origin,
                ray.direction,
                unit.capsule_a,
                unit.capsule_b,
                unit.radius,
            )
        else:
            hit = ray_aabb_intersection(
                ray.origin,
                ray.direction,
                unit.minimum,
                unit.maximum,
            )
        if hit is not None:
            t, normal = hit
            candidates.append((t, 0, unit.unit_id, "unit", normal, point_at(t)))

    for candidate_index in snapshot.cover_bvh.candidates(ray.origin, ray.direction):
        cover = scene.covers[candidate_index]
        a, b, c = scene.triangle(cover)
        hit = ray_triangle_intersection(ray.origin, ray.direction, a, b, c)
        if hit is not None:
            t, normal = hit
            candidates.append((t, 1, cover.cover_id, "cover", normal, point_at(t)))

    ground_hit = ray_plane_z_intersection(ray.origin, ray.direction, scene.ground_height)
    if ground_hit is not None:
        t, normal = ground_hit
        candidates.append((t, 2, 0, "ground", normal, point_at(t)))

    diagnostics: list[PickDiagnostic] = []
    if not candidates:
        hit_result = Hit("none", None, None, None, None)
    else:
        candidates.sort(key=lambda item: (item[0], item[1], item[2]))
        t, _, entity_id, kind, normal, point = candidates[0]
        if near_grazing(ray.direction, normal):
            diagnostics.append(
                PickDiagnostic(
                    W_RAY_GRAZING,
                    "ray is nearly tangent to the hit surface",
                    "hit.normal",
                )
            )
        hit_result = Hit(
            kind=kind,
            entity_id=entity_id,
            point=point,
            normal=normal,
            distance=t,
        )
    return PickResult(
        screen=(float(screen[0]), float(screen[1])),
        ray=ray,
        version=snapshot.version,
        content_hash=snapshot.content_hash,
        hit=hit_result,
        diagnostics=tuple(diagnostics),
    )


def pick_from_handle(
    handle: SnapshotHandle,
    camera: Camera,
    screen: Sequence[float],
    *,
    expected_version: int | None = None,
    expected_hash: str | None = None,
) -> PickResult:
    handle.ensure_active()
    snapshot = handle.snapshot
    if expected_version is not None and expected_version != snapshot.version:
        fail(
            E_VERSION_STALE,
            "snapshot handle version does not match the requested version",
            field="region.version",
            expected=expected_version,
            actual=snapshot.version,
        )
    if expected_hash is not None and expected_hash != snapshot.content_hash:
        fail(
            E_VERSION_STALE,
            "snapshot handle hash does not match the requested hash",
            field="region.content_hash",
            expected=expected_hash,
            actual=snapshot.content_hash,
        )
    return _pick_on_snapshot(snapshot, camera, screen)


def pick_from_store(
    store: VersionStore,
    camera: Camera,
    screen: Sequence[float],
    *,
    expected_version: int | None = None,
    expected_hash: str | None = None,
) -> PickResult:
    handle = store.acquire(
        expected_version=expected_version,
        expected_hash=expected_hash,
    )
    try:
        return pick_from_handle(handle, camera, screen)
    finally:
        handle.release()


def pick_snapshot(
    snapshot: RegionSnapshot,
    camera: Camera,
    screen: Sequence[float],
) -> PickResult:
    return _pick_on_snapshot(snapshot, camera, screen)
