"""Input-entry fault injection. Query and version-management source is untouched."""

from __future__ import annotations

import struct
from dataclasses import replace
from typing import Any, Callable

from .camera import Camera
from .errors import ToolError, fail
from .generator import CASES, _build_initial_scene
from .math3d import Ray
from .query import PickRequest, Picker
from .region import PlacementRequest, RegionJudge
from .scene import HEADER, TRIANGLE_RECORD, UNIT_RECORD, VERTEX_RECORD, Unit, decode_scene, encode_scene
from .snapshot import SceneManager

INJECTION_CASES = (
    "degenerate_bounds",
    "illegal_triangle_index",
    "zero_length_ray",
    "out_of_bounds_screen",
    "self_intersecting_region",
    "corrupt_snapshot",
)

EXPECTED_INJECTION_CODES = {
    "degenerate_bounds": "E_SCENE_INVALID",
    "illegal_triangle_index": "E_SCENE_INVALID",
    "zero_length_ray": "E_RAY_INVALID",
    "out_of_bounds_screen": "E_SCREEN_OUT_OF_RANGE",
    "self_intersecting_region": "E_REGION_INVALID",
    "corrupt_snapshot": "E_SNAPSHOT_CORRUPT",
}


def _scene(seed: int = 20261122):
    spec = CASES[0]
    return _build_initial_scene(spec, seed + spec.seed_offset)


def _degenerate_bounds() -> None:
    scene = _scene()
    original = scene.units[0]
    lower, _upper = original.bounds()
    broken = Unit.box(original.unit_id, lower, lower)
    data = encode_scene(replace(scene, units=(broken, *scene.units[1:])))
    decode_scene(data)


def _illegal_triangle_index() -> None:
    scene = _scene()
    data = bytearray(encode_scene(scene))
    triangle_offset = (
        HEADER.size
        + len(scene.units) * UNIT_RECORD.size
        + len(scene.cover_triangles) * 3 * VERTEX_RECORD.size
    )
    struct.pack_into("<I", data, triangle_offset + 4, 0x7FFFFFFF)
    decode_scene(bytes(data))


def _zero_length_ray() -> None:
    manager = SceneManager(_scene())
    Picker(manager).pick_ray(Ray((1.0, 2.0, 3.0), (0.0, 0.0, 0.0)))


def _out_of_bounds_screen() -> None:
    manager = SceneManager(_scene())
    camera = Camera.look_at((5.0, 8.0, 8.0), (4.0, 0.0, 4.0))
    Picker(manager).pick(PickRequest((-1.0, camera.height * 0.5), camera))


def _self_intersecting_region() -> None:
    manager = SceneManager(_scene())
    polygon = ((1.0, 1.0), (3.0, 3.0), (1.0, 3.0), (3.0, 1.0))
    RegionJudge(manager).judge(PlacementRequest(polygon))


def _corrupt_snapshot() -> None:
    manager = SceneManager(_scene())
    # Lease the snapshot first, then corrupt only its published seal. The
    # picker's normal O(1) handle check must reject it; query source is untouched.
    handle = manager.acquire()
    object.__setattr__(manager.current, "content_hash", "corrupt-" + "0" * 56)
    camera = Camera.look_at((5.0, 8.0, 8.0), (4.0, 0.0, 4.0))
    Picker(manager).pick(
        PickRequest((camera.width * 0.5, camera.height * 0.5), camera),
        handle=handle,
    )


_CASES: dict[str, Callable[[], None]] = {
    "degenerate_bounds": _degenerate_bounds,
    "illegal_triangle_index": _illegal_triangle_index,
    "zero_length_ray": _zero_length_ray,
    "out_of_bounds_screen": _out_of_bounds_screen,
    "self_intersecting_region": _self_intersecting_region,
    "corrupt_snapshot": _corrupt_snapshot,
}


def trigger_injection(name: str, seed: int = 20261122) -> ToolError:
    if name not in _CASES:
        fail("E_USAGE", f"unknown injection case {name!r}", "cli.inject", available=list(INJECTION_CASES))
    try:
        _CASES[name]()
    except ToolError as exc:
        return exc
    fail(
        "E_INJECTION_NOT_TRIGGERED",
        "injection case completed without raising the expected business error",
        f"injection.{name}",
    )


def injection_selfcheck(seed: int = 20261122) -> dict[str, Any]:
    checks = []
    passed = True
    for name in INJECTION_CASES:
        error = trigger_injection(name, seed)
        expected = EXPECTED_INJECTION_CODES[name]
        ok = error.code == expected and bool(error.injection_point)
        passed = passed and ok
        checks.append(
            {
                "case": name,
                "expected_code": expected,
                "actual_code": error.code,
                "injection_point": error.injection_point,
                "message": error.message,
                "ok": ok,
            }
        )
    return {"ok": passed, "checks": checks}

