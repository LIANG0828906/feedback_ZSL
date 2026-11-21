"""Entry-point-only fault injection.

The injection adapter deliberately wraps public loaders and query entry points.
It does not special-case any branch inside the picking or version-management
implementations.
"""

from __future__ import annotations

from dataclasses import replace
from typing import Any

from .camera import Camera
from .errors import (
    E_INJECT_UNKNOWN,
    INJECTION_CODES,
    PickError,
    fail,
)
from .generator import build_dataset
from .model import validate_scene
from .placement import check_placement
from .scene_io import decode_scene, decode_snapshot, encode_snapshot
from .versioning import VersionStore

INJECTION_ALIASES = {
    "degenerate-bounds": "degenerate-bounds",
    "zero-ray": "zero-ray",
    "zero-length-ray": "zero-ray",
    "out-of-range-screen": "out-of-range-screen",
    "screen-range": "out-of-range-screen",
    "self-intersect-region": "self-intersect-region",
    "corrupt-snapshot": "corrupt-snapshot",
}


def _wrap(kind: str, injection_point: str, action: Any) -> None:
    try:
        action()
    except PickError as exc:
        location = dict(exc.location)
        location.update(
            {
                "injection_point": injection_point,
                "injection_kind": kind,
            }
        )
        raise PickError(
            code=INJECTION_CODES[kind],
            message=f"injected {kind} was rejected at the input boundary",
            location=location,
            cause_code=exc.code,
        ) from exc
    fail(
        E_INJECT_UNKNOWN,
        "injection was not detected by the input validation layer",
        field="injection",
        injection_point=injection_point,
        injection_kind=kind,
    )


def run_injection(kind: str, *, seed: int = 7) -> None:
    canonical_kind = INJECTION_ALIASES.get(kind)
    if canonical_kind is None:
        fail(
            E_INJECT_UNKNOWN,
            "unknown injection kind",
            field="--inject",
            injection_kind=kind,
            supported=sorted(INJECTION_CODES),
        )

    bundle = build_dataset(seed)
    first_scene_name = next(
        name for name in bundle.sample_files if name.endswith(".tpscene")
    )
    scene = decode_scene(bundle.sample_files[first_scene_name], source=first_scene_name)
    scenario_name = next(
        name for name in bundle.sample_files if name.endswith(".scenario.json")
    )
    import json

    scenario = json.loads(bundle.sample_files[scenario_name].decode("utf-8"))
    camera = Camera.from_dict(scenario["camera"]).validate()
    store = VersionStore(scene)

    if canonical_kind == "degenerate-bounds":
        unit = scene.units[0]
        degenerate = replace(unit, minimum=unit.maximum)
        _wrap(
            canonical_kind,
            "scene.units[0].maximum",
            lambda: validate_scene(replace(scene, units=(degenerate, *scene.units[1:]))),
        )
        return

    if canonical_kind == "zero-ray":
        zero_camera = replace(camera, position=camera.target)
        _wrap(
            canonical_kind,
            "camera.position/target",
            lambda: zero_camera.screen_to_ray(
                [camera.viewport_width * 0.5, camera.viewport_height * 0.5]
            ),
        )
        return

    if canonical_kind == "out-of-range-screen":
        _wrap(
            canonical_kind,
            "screen[-1,0]",
            lambda: camera.screen_to_ray([-1.0, 0.0]),
        )
        return

    if canonical_kind == "self-intersect-region":
        _wrap(
            canonical_kind,
            "region.vertices[2]",
            lambda: check_placement(
                store,
                [[1.0, 1.0], [3.0, 3.0], [1.0, 3.0], [3.0, 1.0]],
            ),
        )
        return

    if canonical_kind == "corrupt-snapshot":
        snapshot = store.current_snapshot()
        encoded = encode_snapshot(
            snapshot.version,
            snapshot.content_hash,
            snapshot.scene,
        )
        corrupted = encoded[:-5] + b"broken"
        _wrap(
            canonical_kind,
            "snapshot.header.payload_length",
            lambda: decode_snapshot(corrupted, source="injected.snapshot"),
        )
        return

    raise AssertionError(f"unhandled injection kind: {canonical_kind}")
