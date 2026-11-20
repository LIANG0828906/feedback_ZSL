"""Deterministic local scene generators; no network or external assets."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def room_scene_payload() -> dict[str, Any]:
    return {
        "scene_id": "street_corner_room",
        "bounds": {"min": [-5.0, 0.0, -4.0], "max": [5.0, 3.0, 5.0]},
        "spacing": 1.0,
        "occluders": [
            {
                "id": "back_wall",
                "type": "aabb",
                "min": [-5.0, 0.0, 4.4],
                "max": [5.0, 3.0, 5.0],
            },
            {
                "id": "service_counter",
                "type": "aabb",
                "min": [-3.0, 0.0, -1.2],
                "max": [1.5, 1.4, 0.2],
            },
            {
                "id": "street_lamp_head",
                "type": "sphere",
                "center": [2.8, 2.4, -2.3],
                "radius": 0.55,
            },
        ],
        "material": {"albedo": [0.72, 0.69, 0.64]},
        "sky": {
            "zenith": [0.78, 0.88, 1.0],
            "horizon": [0.92, 0.86, 0.74],
            "ground": [0.12, 0.11, 0.09],
            "intensity": 1.15,
        },
        "dynamic_mask": [],
    }


def city_block_payload() -> dict[str, Any]:
    occluders: list[dict[str, Any]] = []
    index = 0
    for x_index, x_value in enumerate((-10.0, -2.0, 6.0)):
        for z_index, z_value in enumerate((-8.0, 0.0, 8.0)):
            height = 2.0 + ((x_index * 3 + z_index) % 4)
            occluders.append(
                {
                    "id": f"block_{index:02d}",
                    "type": "aabb",
                    "min": [x_value, 0.0, z_value],
                    "max": [x_value + 3.0, height, z_value + 3.0],
                }
            )
            index += 1
    occluders.extend(
        [
            {
                "id": "plaza_tree_a",
                "type": "sphere",
                "center": [1.0, 2.0, -4.0],
                "radius": 1.0,
            },
            {
                "id": "plaza_tree_b",
                "type": "sphere",
                "center": [2.5, 1.8, 4.0],
                "radius": 0.8,
            },
        ]
    )
    return {
        "scene_id": "city_block_demo",
        "bounds": {"min": [-12.0, 0.0, -10.0], "max": [12.0, 5.0, 12.0]},
        "spacing": 2.0,
        "occluders": occluders,
        "material": {"albedo": [0.62, 0.66, 0.70]},
        "sky": {
            "zenith": [0.64, 0.78, 1.0],
            "horizon": [0.88, 0.78, 0.61],
            "ground": [0.10, 0.09, 0.08],
            "intensity": 1.3,
        },
        "dynamic_mask": ["plaza_tree_b"],
    }


def write_sample_scenes(directory: str | Path) -> tuple[Path, Path]:
    root = Path(directory)
    root.mkdir(parents=True, exist_ok=True)
    room_path = root / "street_room.json"
    city_path = root / "city_block.json"
    room_path.write_text(
        json.dumps(room_scene_payload(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    city_path.write_text(
        json.dumps(city_block_payload(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return room_path, city_path

