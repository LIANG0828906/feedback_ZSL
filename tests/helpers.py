from __future__ import annotations

import copy
import json
from pathlib import Path

from gallery_louvers.scene import Scene, validate_scene
from gallery_louvers.scene_builder import build_scene


def small_scene_raw(
    width: int = 96,
    height: int = 72,
    angles: list[float] | None = None,
) -> dict:
    raw = copy.deepcopy(build_scene())
    raw["camera"]["width"] = width
    raw["camera"]["height"] = height
    raw["camera"]["angles_deg"] = angles or [-30.0, 0.0, 30.0]
    raw["render"]["tile_width"] = 32
    raw["render"]["tile_height"] = 24
    raw["render"]["pattern_resolution"] = [64, 64]
    raw["render"]["microsurface_resolution"] = [16, 8]
    return raw


def small_scene() -> Scene:
    return validate_scene(small_scene_raw())


def write_scene(path: str | Path, raw: dict) -> Path:
    path = Path(path)
    path.write_text(
        json.dumps(raw, ensure_ascii=False, sort_keys=True),
        encoding="utf-8",
    )
    return path
