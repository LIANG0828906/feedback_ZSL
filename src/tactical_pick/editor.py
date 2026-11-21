"""Batch edits built off to the side, followed by one atomic publication."""

from __future__ import annotations

import math
from dataclasses import replace
from typing import Any, Iterable

from .errors import fail
from .scene import CoverTriangle, SceneData, Unit, set_mask_bit
from .snapshot import SceneManager, SceneSnapshot
from .util import f32


class SceneEditor:
    def __init__(self, manager: SceneManager) -> None:
        self.manager = manager

    def apply_batch(self, operations: Iterable[dict[str, Any]]) -> SceneSnapshot:
        with self.manager.edit_lock:
            base = self.manager.current.scene
            units = list(base.units)
            triangles = list(base.cover_triangles)
            mask = base.passable_mask
            version = base.version + 1

            for index, operation in enumerate(operations):
                point = f"batch.operations[{index}]"
                if not isinstance(operation, dict):
                    fail("E_SCENE_INVALID", "edit operation must be an object", point)
                kind = operation.get("op")
                if kind in ("move_unit", "set_unit_delta"):
                    unit_id = int(operation.get("unit_id", -1))
                    delta = operation.get("delta", [0.0, 0.0, 0.0])
                    if len(delta) != 3:
                        fail("E_SCENE_INVALID", "unit delta must have three components", f"{point}.delta")
                    try:
                        values = [float(delta[0]), float(delta[1]), float(delta[2])]
                    except (TypeError, ValueError):
                        fail("E_SCENE_INVALID", "unit delta is not numeric", f"{point}.delta")
                    if not all(math.isfinite(value) for value in values):
                        fail("E_SCENE_INVALID", "unit delta is not finite", f"{point}.delta")
                    found = False
                    for unit_index, unit in enumerate(units):
                        if unit.unit_id == unit_id:
                            units[unit_index] = unit.translated(values)
                            found = True
                            break
                    if not found:
                        fail(
                            "E_SCENE_INVALID",
                            "edit refers to an unknown unit",
                            f"{point}.unit_id",
                            first_conflict={"index": index, "unit_id": unit_id},
                        )
                elif kind == "set_walkable":
                    x = int(operation.get("x", -1))
                    z = int(operation.get("z", -1))
                    walkable = bool(operation.get("walkable", False))
                    if x < 0 or z < 0 or x >= base.grid_width or z >= base.grid_height:
                        fail(
                            "E_SCENE_INVALID",
                            "walkability edit is outside the grid",
                            f"{point}.cell",
                            first_conflict={"index": index, "x": x, "z": z},
                        )
                    mask = set_mask_bit(mask, base.grid_width, x, z, walkable)
                elif kind == "add_cover_triangle":
                    try:
                        cover_id = int(operation["cover_id"])
                        a = tuple(float(value) for value in operation["a"])
                        b = tuple(float(value) for value in operation["b"])
                        c = tuple(float(value) for value in operation["c"])
                    except (KeyError, TypeError, ValueError):
                        fail("E_SCENE_INVALID", "cover edit is malformed", point)
                    if len(a) != 3 or len(b) != 3 or len(c) != 3:
                        fail("E_SCENE_INVALID", "cover vertices must have three components", point)
                    if not all(math.isfinite(value) for value in (*a, *b, *c)):
                        fail("E_SCENE_INVALID", "cover vertex is not finite", point)
                    triangles.append(
                        CoverTriangle(
                            cover_id,
                            (f32(a[0]), f32(a[1]), f32(a[2])),
                            (f32(b[0]), f32(b[1]), f32(b[2])),
                            (f32(c[0]), f32(c[1]), f32(c[2])),
                        )
                    )
                else:
                    fail(
                        "E_SCENE_INVALID",
                        f"unknown edit operation {kind!r}",
                        f"{point}.op",
                        first_conflict={"index": index},
                    )

            scene = SceneData(
                version=version,
                grid_width=base.grid_width,
                grid_height=base.grid_height,
                cell_size=base.cell_size,
                ground_y=base.ground_y,
                units=tuple(units),
                cover_triangles=tuple(triangles),
                passable_mask=mask,
            )
            snapshot = SceneSnapshot.from_scene(scene)
            self.manager.publish(snapshot)
            return snapshot

    def apply_batches(self, batches: Iterable[Iterable[dict[str, Any]]]) -> list[SceneSnapshot]:
        return [self.apply_batch(batch) for batch in batches]

