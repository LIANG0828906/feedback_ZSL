"""Immutable region snapshots, handles and atomic batch publication."""

from __future__ import annotations

import math
import threading
from dataclasses import dataclass, replace
from typing import Any, Mapping, Sequence

from .bvh import Bounds, BVH
from .errors import (
    E_BATCH_CONFLICT,
    E_SCENE_INVALID,
    E_STALE_HANDLE,
    E_VERSION_STALE,
    PickError,
    fail,
)
from .geometry import Vec3
from .model import (
    Cover,
    Scene,
    Unit,
    scene_replace_covers,
    scene_replace_units,
    scene_with_grid,
    validate_scene,
)
from .scene_io import scene_content_hash


def _unit_bounds(unit: Unit) -> Bounds:
    return Bounds(unit.minimum, unit.maximum)


def _cover_bounds(scene: Scene, cover: Cover) -> Bounds:
    a, b, c = scene.triangle(cover)
    return Bounds(
        (min(a[0], b[0], c[0]), min(a[1], b[1], c[1]), min(a[2], b[2], c[2])),
        (max(a[0], b[0], c[0]), max(a[1], b[1], c[1]), max(a[2], b[2], c[2])),
    )


@dataclass(frozen=True)
class RegionSnapshot:
    version: int
    content_hash: str
    scene: Scene
    unit_bvh: BVH
    cover_bvh: BVH

    @property
    def structure_version(self) -> int:
        return self.scene.structure_version

    def version_key(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "structure_version": self.scene.structure_version,
        }


def build_snapshot(scene: Scene, version: int) -> RegionSnapshot:
    validate_scene(scene)
    if version < 1:
        fail(E_SCENE_INVALID, "region version must be positive", field="region.version")
    unit_bounds = [_unit_bounds(unit) for unit in scene.active_units()]
    cover_bounds = [_cover_bounds(scene, cover) for cover in scene.covers]
    return RegionSnapshot(
        version=version,
        content_hash=scene_content_hash(scene),
        scene=scene,
        unit_bvh=BVH.build(unit_bounds),
        cover_bvh=BVH.build(cover_bounds),
    )


class SnapshotHandle:
    """A pinned snapshot handle.

    Existing handles may continue reading their complete immutable snapshot
    while a replacement is published.  Once released, every query through the
    handle fails with ``E_STALE_HANDLE``.
    """

    __slots__ = ("_store", "snapshot", "serial", "_released")

    def __init__(self, store: "VersionStore", snapshot: RegionSnapshot, serial: int):
        self._store = store
        self.snapshot = snapshot
        self.serial = serial
        self._released = False

    @property
    def released(self) -> bool:
        return self._released

    def ensure_active(self) -> None:
        if self._released:
            fail(
                E_STALE_HANDLE,
                "snapshot handle was already released",
                field="snapshot_handle",
                handle_serial=self.serial,
                version=self.snapshot.version,
            )
        self._store._assert_handle_registered(self)

    def release(self) -> None:
        if self._released:
            return
        self._store._release_handle(self)

    def __enter__(self) -> "SnapshotHandle":
        self.ensure_active()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.release()


class VersionStore:
    """Publish complete snapshots with a very short lock critical section."""

    def __init__(self, scene: Scene):
        snapshot = build_snapshot(scene, 1)
        self._lock = threading.RLock()
        self._current = snapshot
        self._history: dict[int, RegionSnapshot] = {1: snapshot}
        self._handles: dict[int, SnapshotHandle] = {}
        self._next_handle_serial = 1

    @property
    def current_version(self) -> int:
        with self._lock:
            return self._current.version

    @property
    def current_hash(self) -> str:
        with self._lock:
            return self._current.content_hash

    def current_snapshot(self) -> RegionSnapshot:
        with self._lock:
            return self._current

    def acquire(
        self,
        *,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> SnapshotHandle:
        with self._lock:
            snapshot = self._current
            if expected_version is not None and expected_version != snapshot.version:
                fail(
                    E_VERSION_STALE,
                    "requested region version is not the published version",
                    field="region.version",
                    expected=expected_version,
                    current=snapshot.version,
                )
            if expected_hash is not None and expected_hash != snapshot.content_hash:
                fail(
                    E_VERSION_STALE,
                    "requested region hash is not the published hash",
                    field="region.content_hash",
                    expected=expected_hash,
                    current=snapshot.content_hash,
                )
            handle = SnapshotHandle(self, snapshot, self._next_handle_serial)
            self._next_handle_serial += 1
            self._handles[handle.serial] = handle
            return handle

    def _assert_handle_registered(self, handle: SnapshotHandle) -> None:
        with self._lock:
            if self._handles.get(handle.serial) is not handle:
                fail(
                    E_STALE_HANDLE,
                    "snapshot handle is not live",
                    field="snapshot_handle",
                    handle_serial=handle.serial,
                    version=handle.snapshot.version,
                )

    def _release_handle(self, handle: SnapshotHandle) -> None:
        with self._lock:
            if self._handles.pop(handle.serial, None) is None and handle.released:
                handle._released = True
                return
            handle._released = True

    def handle_from_snapshot(self, snapshot: RegionSnapshot) -> SnapshotHandle:
        with self._lock:
            handle = SnapshotHandle(self, snapshot, self._next_handle_serial)
            self._next_handle_serial += 1
            self._handles[handle.serial] = handle
            return handle

    def snapshot_for_version(
        self,
        version: int,
        *,
        expected_hash: str | None = None,
    ) -> RegionSnapshot:
        with self._lock:
            snapshot = self._history.get(version)
        if snapshot is None:
            fail(
                E_VERSION_STALE,
                "region version is not available in history",
                field="region.version",
                version=version,
                available=sorted(self.versions()),
            )
        if expected_hash is not None and snapshot.content_hash != expected_hash:
            fail(
                E_VERSION_STALE,
                "region version content hash does not match",
                field="region.content_hash",
                version=version,
                expected=expected_hash,
                actual=snapshot.content_hash,
            )
        return snapshot

    def versions(self) -> tuple[int, ...]:
        with self._lock:
            return tuple(sorted(self._history))

    def history(self) -> tuple[RegionSnapshot, ...]:
        with self._lock:
            return tuple(self._history[key] for key in sorted(self._history))

    def _publish_if_current(
        self,
        candidate: RegionSnapshot,
        *,
        expected_version: int,
    ) -> RegionSnapshot:
        with self._lock:
            current = self._current
            if current.version != expected_version:
                fail(
                    E_VERSION_STALE,
                    "published region changed while the batch was being prepared",
                    field="region.version",
                    expected=expected_version,
                    current=current.version,
                )
            if candidate.version != expected_version + 1:
                fail(
                    E_BATCH_CONFLICT,
                    "candidate region version is not the next monotonic version",
                    field="region.version",
                    expected=expected_version + 1,
                    actual=candidate.version,
                )
            self._current = candidate
            self._history[candidate.version] = candidate
            return candidate

    def publish_scene(
        self,
        scene: Scene,
        *,
        expected_version: int | None = None,
    ) -> RegionSnapshot:
        with self._lock:
            base = self._current
        if expected_version is not None and base.version != expected_version:
            fail(
                E_VERSION_STALE,
                "requested base region version is stale",
                field="region.version",
                expected=expected_version,
                current=base.version,
            )
        candidate = build_snapshot(scene, base.version + 1)
        return self._publish_if_current(candidate, expected_version=base.version)

    def apply_batch(
        self,
        operations: Sequence[Mapping[str, Any]],
        *,
        expected_version: int | None = None,
    ) -> RegionSnapshot:
        """Apply all operations off-lock and publish only after full success.

        The published state is never mutated in place.  A validation failure in
        any operation leaves the previous snapshot as the sole current version.
        """

        with self._lock:
            base = self._current
        if expected_version is not None and base.version != expected_version:
            fail(
                E_VERSION_STALE,
                "batch base region version is stale",
                field="region.version",
                expected=expected_version,
                current=base.version,
            )
        scene = base.scene
        for index, operation in enumerate(operations):
            try:
                scene = _apply_operation(scene, operation)
            except PickError as exc:
                location = dict(exc.location)
                location.update(
                    {
                        "operation_index": index,
                        "operation": str(operation.get("op", "<missing>")),
                        "batch_base_version": base.version,
                    }
                )
                raise PickError(
                    code=exc.code,
                    message=f"batch operation {index} failed: {exc.message}",
                    location=location,
                    cause_code=exc.cause_code,
                ) from exc
            except Exception as exc:
                fail(
                    E_SCENE_INVALID,
                    "batch operation is malformed",
                    field=f"operations[{index}]",
                    operation_index=index,
                    error=str(exc),
                )
        try:
            validate_scene(scene)
            candidate = build_snapshot(scene, base.version + 1)
        except PickError as exc:
            location = dict(exc.location)
            location.update(
                {
                    "operation_index": len(operations) - 1,
                    "batch_base_version": base.version,
                    "phase": "validate_and_rebuild",
                }
            )
            raise PickError(
                code=exc.code,
                message=f"batch validation failed: {exc.message}",
                location=location,
                cause_code=exc.cause_code,
            ) from exc
        except Exception as exc:
            fail(
                E_SCENE_INVALID,
                "batch validation or acceleration rebuild failed",
                field="batch",
                batch_base_version=base.version,
                phase="validate_and_rebuild",
                error=str(exc),
            )
        return self._publish_if_current(candidate, expected_version=base.version)


def _require_int(operation: Mapping[str, Any], field: str) -> int:
    value = operation[field]
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{field} must be an integer")
    return value


def _require_vec3(operation: Mapping[str, Any], field: str) -> Vec3:
    value = operation[field]
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise ValueError(f"{field} must be a three-element vector")
    result = (float(value[0]), float(value[1]), float(value[2]))
    if not all(math.isfinite(item) for item in result):
        raise ValueError(f"{field} must be finite")
    return result


def _operation_unit(scene: Scene, operation: Mapping[str, Any]) -> tuple[int, Unit, int]:
    unit_id = _require_int(operation, "unit_id")
    for index, unit in enumerate(scene.units):
        if unit.unit_id == unit_id:
            return index, unit, unit_id
    fail(
        E_SCENE_INVALID,
        "batch references an unknown unit id",
        field="unit_id",
        entity_id=unit_id,
    )


def _operation_cover(scene: Scene, operation: Mapping[str, Any]) -> tuple[int, Cover, int]:
    cover_id = _require_int(operation, "cover_id")
    for index, cover in enumerate(scene.covers):
        if cover.cover_id == cover_id:
            return index, cover, cover_id
    fail(
        E_SCENE_INVALID,
        "batch references an unknown cover id",
        field="cover_id",
        entity_id=cover_id,
    )


def _unit_from_spec(spec: Mapping[str, Any]) -> Unit:
    if not isinstance(spec, Mapping):
        raise ValueError("unit specification must be an object")
    bounds = spec.get("bounds", {})
    if not isinstance(bounds, Mapping):
        raise ValueError("unit bounds specification must be an object")
    kind = str(bounds.get("kind", spec.get("bounds_kind", "aabb")))
    minimum = _spec_vec3(bounds["minimum"], "bounds.minimum")
    maximum = _spec_vec3(bounds["maximum"], "bounds.maximum")
    capsule_a = None
    capsule_b = None
    radius = 0.0
    if kind == "capsule":
        capsule_a = _spec_vec3(bounds["capsule_a"], "bounds.capsule_a")
        capsule_b = _spec_vec3(bounds["capsule_b"], "bounds.capsule_b")
        radius = float(bounds["radius"])
    return Unit(
        unit_id=int(spec["unit_id"]),
        name=str(spec.get("name", f"unit-{spec['unit_id']}")),
        bounds_kind=kind,
        minimum=minimum,
        maximum=maximum,
        capsule_a=capsule_a,
        capsule_b=capsule_b,
        radius=radius,
        active=bool(spec.get("active", True)),
    )


def _cover_from_spec(spec: Mapping[str, Any]) -> Cover:
    if not isinstance(spec, Mapping):
        raise ValueError("cover specification must be an object")
    indices = spec["indices"]
    if not isinstance(indices, (list, tuple)) or len(indices) != 3:
        raise ValueError("cover indices must contain three values")
    return Cover(
        cover_id=int(spec["cover_id"]),
        i0=int(indices[0]),
        i1=int(indices[1]),
        i2=int(indices[2]),
        name=str(spec.get("name", f"cover-{spec['cover_id']}")),
    )


def _spec_vec3(value: Any, field: str) -> Vec3:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise ValueError(f"{field} must contain three coordinates")
    result = (float(value[0]), float(value[1]), float(value[2]))
    if not all(math.isfinite(item) for item in result):
        raise ValueError(f"{field} must be finite")
    return result


def _apply_operation(scene: Scene, operation: Mapping[str, Any]) -> Scene:
    if not isinstance(operation, Mapping):
        raise ValueError("operation must be an object")
    op = operation.get("op")
    if op == "translate_unit":
        _, unit, unit_id = _operation_unit(scene, operation)
        delta = _require_vec3(operation, "delta")
        moved = unit.translated(delta)
        return scene_replace_units(
            scene,
            tuple(moved if current.unit_id == unit_id else current for current in scene.units),
        )
    if op == "set_unit_position":
        _, unit, unit_id = _operation_unit(scene, operation)
        target = _require_vec3(operation, "position")
        center = (
            (unit.minimum[0] + unit.maximum[0]) * 0.5,
            (unit.minimum[1] + unit.maximum[1]) * 0.5,
            (unit.minimum[2] + unit.maximum[2]) * 0.5,
        )
        moved = unit.translated(
            (target[0] - center[0], target[1] - center[1], target[2] - center[2])
        )
        return scene_replace_units(
            scene,
            tuple(moved if current.unit_id == unit_id else current for current in scene.units),
        )
    if op == "set_unit_active":
        _, unit, unit_id = _operation_unit(scene, operation)
        active = bool(operation["active"])
        return scene_replace_units(
            scene,
            tuple(
                replace(current, active=active) if current.unit_id == unit_id else current
                for current in scene.units
            ),
        )
    if op == "set_unit_bounds":
        _, unit, unit_id = _operation_unit(scene, operation)
        minimum = _require_vec3(operation, "minimum")
        maximum = _require_vec3(operation, "maximum")
        return scene_replace_units(
            scene,
            tuple(
                replace(current, minimum=minimum, maximum=maximum)
                if current.unit_id == unit_id
                else current
                for current in scene.units
            ),
        )
    if op == "replace_units":
        specs = operation["units"]
        return scene_replace_units(scene, tuple(_unit_from_spec(spec) for spec in specs))
    if op == "set_walkable_cells":
        grid = scene.grid
        for cell in operation["cells"]:
            grid = grid.set_walkable(
                int(cell["row"]),
                int(cell["col"]),
                bool(cell["walkable"]),
            )
        return scene_with_grid(scene, grid)
    if op == "set_walkable_mask":
        raw = operation["hex"]
        if not isinstance(raw, str):
            raise ValueError("hex must be a string")
        try:
            mask = bytes.fromhex(raw)
        except ValueError as exc:
            raise ValueError("hex mask is malformed") from exc
        return scene_with_grid(scene, scene.grid.with_walkable_mask(mask))
    if op == "replace_cover_geometry":
        vertices = tuple(
            (float(vertex[0]), float(vertex[1]), float(vertex[2]))
            for vertex in operation["vertices"]
        )
        covers = tuple(_cover_from_spec(spec) for spec in operation["covers"])
        return scene_replace_covers(scene, vertices, covers)
    if op == "set_cover_vertices":
        _, cover, cover_id = _operation_cover(scene, operation)
        vertices = list(scene.vertices)
        for label, index in (("i0", cover.i0), ("i1", cover.i1), ("i2", cover.i2)):
            vertices[index] = _require_vec3(operation, label)
        return scene_replace_covers(scene, vertices, scene.covers)
    if op == "set_ground_height":
        height = float(operation["height"])
        return replace(scene, ground_height=height)
    if op == "set_structure_version":
        value = int(operation["value"])
        return replace(scene, structure_version=value)
    fail(
        E_SCENE_INVALID,
        "unknown batch operation",
        field="operations.op",
        operation=str(op),
    )
