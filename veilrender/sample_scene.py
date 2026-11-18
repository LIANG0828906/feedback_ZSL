"""Deterministic demo scene generation (no randomness or external assets)."""

from __future__ import annotations

import copy
from pathlib import Path
from typing import Any

from .scene import SCENE_SCHEMA, canonical_json, validate_scene


def _panel_mesh(x0: float, x1: float, y0: float, y1: float, z: float) -> dict[str, Any]:
    """A planar two-triangle panel with consistent outward winding."""

    vertices = [
        [x0, y0, z],
        [x1, y0, z],
        [x1, y1, z],
        [x0, y1, z],
    ]
    return {
        "vertices": vertices,
        "triangles": [[0, 1, 2], [0, 2, 3]],
    }


def make_sample_scene(
    *,
    width: int = 256,
    height: int = 160,
    tile_width: int = 64,
    tile_height: int = 40,
    camera_samples: int = 2,
) -> dict[str, Any]:
    scene: dict[str, Any] = {
        "schema_version": SCENE_SCHEMA,
        "name": "courtyard-night-gauze-audit",
        "image": {
            "width": width,
            "height": height,
            "tile_width": tile_width,
            "tile_height": tile_height,
            "background_rgba": [0.012, 0.017, 0.035, 1.0],
        },
        "camera": {
            "position": [0.0, 1.65, 6.3],
            "target": [0.0, 1.28, 0.0],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 43.0,
            "near": 0.03,
            "far": 20.0,
            "samples": camera_samples,
        },
        "global": {
            "ambient": 0.045,
            "light_falloff": 0.075,
            "scatter_strength": 0.68,
            "shadow_strength": 0.86,
        },
        "projection": {
            "samples_per_axis": 3,
            "landing_samples_per_axis": 3,
            "penumbra_base": 0.010,
            "penumbra_distance_gain": 0.36,
            "penumbra_source_gain": 0.085,
            "max_penumbra": 0.17,
        },
        "ground": {
            "y": 0.0,
            "x": [-4.5, 4.5],
            "z": [-3.5, 5.2],
            "color": [0.16, 0.18, 0.20],
            "reflectance": 0.34,
        },
        "lights": [
            {
                "id": "L-key",
                "position": [-2.1, 3.15, -2.0],
                "intensity": 7.0,
                "color": [1.0, 0.78, 0.48],
            },
            {
                "id": "L-fill",
                "position": [2.4, 2.15, 4.4],
                "intensity": 2.8,
                "color": [0.54, 0.72, 1.0],
            },
            {
                "id": "L-practical",
                "position": [0.15, 0.55, -1.25],
                "intensity": 2.2,
                "color": [1.0, 0.38, 0.22],
            },
        ],
        "layers": [
            {
                "id": "inner-teal-gauze",
                "layer": 1,
                "kind": "gauze",
                "mesh": _panel_mesh(-2.75, 2.75, 0.42, 2.78, 0.35),
                "transmittance": 0.56,
                "tint": [0.30, 0.78, 0.74],
                "weave_seed": 1103,
                "resolution": [64, 64],
                "texture_repeat": 8,
                "thickness": 0.005,
            },
            {
                "id": "inner-silver-lattice",
                "layer": 2,
                "kind": "lattice",
                "mesh": _panel_mesh(-2.55, 2.55, 0.40, 2.70, 0.90),
                "reflectance": 0.72,
                "width": 0.021,
                "metal_color": [0.76, 0.68, 0.49],
                "pattern_seed": 29,
                "pattern": {
                    "style": "diamond",
                    "pitch": 0.245,
                    "edge_softness": 0.0018,
                },
                "thickness": 0.01,
            },
            {
                "id": "middle-rose-gauze",
                "layer": 3,
                "kind": "gauze",
                "mesh": _panel_mesh(-2.62, 2.62, 0.41, 2.72, 1.58),
                "transmittance": 0.47,
                "tint": [0.82, 0.31, 0.37],
                "weave_seed": 2207,
                "resolution": [64, 64],
                "texture_repeat": 9,
                "thickness": 0.006,
            },
            {
                "id": "rose-gauze-pair",
                "layer": 4,
                "kind": "gauze",
                "mesh": _panel_mesh(-2.68, 2.68, 0.39, 2.74, 1.96),
                "transmittance": 0.44,
                "tint": [0.82, 0.31, 0.37],
                "weave_seed": 2411,
                "resolution": [80, 80],
                "texture_repeat": 11,
                "thickness": 0.006,
            },
            {
                "id": "outer-bronze-lattice",
                "layer": 5,
                "kind": "lattice",
                "mesh": _panel_mesh(-2.45, 2.45, 0.40, 2.67, 2.40),
                "reflectance": 0.61,
                "width": 0.026,
                "metal_color": [0.70, 0.51, 0.31],
                "pattern_seed": 71,
                "pattern": {
                    "style": "grid",
                    "pitch": 0.31,
                    "edge_softness": 0.0022,
                },
                "thickness": 0.012,
            },
            {
                "id": "outer-rose-gauze",
                "layer": 6,
                "kind": "gauze",
                "mesh": _panel_mesh(-2.72, 2.72, 0.38, 2.75, 2.95),
                "transmittance": 0.39,
                "tint": [0.73, 0.52, 0.54],
                "weave_seed": 3301,
                "resolution": [96, 96],
                "texture_repeat": 10,
                "thickness": 0.007,
            },
        ],
    }
    return validate_scene(scene)


def make_invalid_scene(case: str) -> dict[str, Any]:
    """Build deterministic invalid fixtures used by tests and demos."""

    scene = make_sample_scene(width=48, height=32, tile_width=16, tile_height=16, camera_samples=1)
    scene = copy.deepcopy(scene)
    if case == "mesh_index":
        scene["layers"][1]["mesh"]["triangles"][0][2] = 999
    elif case == "degenerate":
        scene["layers"][0]["mesh"]["triangles"][0] = [0, 1, 1]
    elif case == "transmittance":
        scene["layers"][0]["transmittance"] = 1.25
    elif case == "reflectance":
        scene["layers"][1]["reflectance"] = -0.1
    elif case == "duplicate_layer":
        scene["layers"][2]["layer"] = scene["layers"][0]["layer"]
    elif case == "duplicate_id":
        scene["layers"][2]["id"] = scene["layers"][0]["id"]
    elif case == "light_inside":
        scene["lights"][0]["position"] = [0.0, 1.5, scene["layers"][2]["mesh"]["vertices"][0][2]]
    elif case == "camera":
        scene["camera"]["fov_y_deg"] = 190.0
    else:
        raise ValueError(f"unknown invalid fixture {case!r}")
    return scene


def write_scene(scene: dict[str, Any], path: str | Path) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(canonical_json(scene) + "\n", encoding="utf-8")

