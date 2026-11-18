"""Deterministic construction of the rose window JSON scene.

The scene is built entirely from integer parameters and trigonometric
geometry: no external model, texture or network resource is involved. The
schema mirrors ``cathedral-rose/v1``.
"""

from __future__ import annotations

import copy
import hashlib
import json
import math
from pathlib import Path
from typing import Any

WINDOW_RADIUS = 2.0
INNER_RADIUS = 0.24
RADII = (INNER_RADIUS, 1.105, 1.97)
FACETS_PER_QUADRANT = 5
ANGLE_STEP_DEG = 18.0

QUADRANT_BASE_COLORS = (
    (0.092, 0.276, 0.9016),   # cobalt blue
    (0.8832, 0.069, 0.1104),  # ruby red
    (0.8648, 0.46, 0.0506),   # amber
    (0.0506, 0.6624, 0.3496), # emerald green
)
TRANSMITTANCE_CYCLE = (0.58, 0.605, 0.63, 0.655, 0.68)
REFLECTANCE_CYCLE = (0.042, 0.049, 0.056, 0.063, 0.07)
LEAD_WIDTH_CYCLE = (0.026, 0.029, 0.032, 0.035, 0.026)
LEAD_DEPTH_CYCLE = (0.03, 0.06, 0.11, 0.045, 0.155)
COLOR_FACTOR_CYCLE = (1.0, 1.0272, 1.0544, 1.0, 1.0272)

TRIANGLES = (
    (0, 3, 1),
    (1, 3, 4),
    (1, 4, 2),
    (2, 4, 5),
    (3, 6, 4),
    (4, 6, 7),
    (4, 7, 5),
    (5, 7, 8),
)


def _rotate_xy(x: float, y: float, quadrant: int) -> tuple[float, float]:
    angle = quadrant * math.pi / 2.0
    cos_a, sin_a = math.cos(angle), math.sin(angle)
    return x * cos_a - y * sin_a, x * sin_a + y * cos_a


def _facet_vertices(quadrant: int, facet: int) -> list[list[float]]:
    start = math.radians((facet - 1) * ANGLE_STEP_DEG)
    end = math.radians(facet * ANGLE_STEP_DEG)
    angles = (start, (start + end) / 2.0, end)
    local: list[tuple[float, float]] = []
    for radius in RADII:
        for angle in angles:
            local.append((radius * math.cos(angle), radius * math.sin(angle)))
    return [[rx, ry, 0.0] for rx, ry in (_rotate_xy(x, y, quadrant) for x, y in local)]


def build_scene(seed: int = 208) -> dict[str, Any]:
    """Construct the full deterministic scene dictionary."""

    quadrants: list[dict[str, Any]] = []
    for quadrant in range(4):
        base = QUADRANT_BASE_COLORS[quadrant]
        facets: list[dict[str, Any]] = []
        for facet_index in range(FACETS_PER_QUADRANT):
            cycle_index = (facet_index + quadrant) % FACETS_PER_QUADRANT
            color_factor = COLOR_FACTOR_CYCLE[cycle_index]
            color = [min(1.0, channel * color_factor) for channel in base]
            facets.append(
                {
                    "facet": facet_index + 1,
                    "transmission_rgb": [round(channel, 6) for channel in color],
                    "transmittance": TRANSMITTANCE_CYCLE[cycle_index],
                    "glaze_reflectance": REFLECTANCE_CYCLE[cycle_index],
                    "lead_width": LEAD_WIDTH_CYCLE[cycle_index],
                    "lead_depth": LEAD_DEPTH_CYCLE[cycle_index],
                    "vertices": _facet_vertices(quadrant, facet_index + 1),
                    "triangles": [list(tri) for tri in TRIANGLES],
                }
            )
        quadrants.append({"quadrant": quadrant, "facets": facets})

    return {
        "schema": "cathedral-rose/v1",
        "seed": seed,
        "window": {"radius": WINDOW_RADIUS},
        "camera": {
            "eye": [0.0, -0.22, 1.65],
            "target": [0.0, 0.0, 0.0],
            "up": [0.0, 1.0, 0.0],
            "vertical_fov_deg": 108.0,
        },
        "directional_light": {
            "direction": [0.3, 0.12, 0.95],
            "color": [1.0, 0.965, 0.9],
            "irradiance": 1.0,
        },
        "interior_light": {
            "direction": [-0.25, 0.08, -0.965],
            "color": [0.5, 0.58, 0.74],
            "irradiance": 0.2,
        },
        "glass": {
            "thickness": 0.018,
            "background_color": [0.012, 0.016, 0.024],
            "lead_color": [0.014, 0.013, 0.012],
            "reflection_tint": [0.66, 0.82, 0.69],
            "reflection_gain": 0.58,
        },
        "texture": {"table_size": 32, "scale": 3.0},
        "render": {
            "width": 256,
            "height": 256,
            "chunk_size": 32,
            "gamma": "sqrt",
            "primary_samples": 2,
        },
        "projection": {
            "wall_z": 4.5,
            "center": [0.62, 0.22],
            "extent": 6.0,
            "width": 256,
            "height": 256,
            "samples_per_pixel": 16,
            "penumbra_scale": 0.7,
            "profile_samples": 257,
        },
        "quadrants": quadrants,
    }


def scene_digest(scene: dict[str, Any]) -> str:
    """Stable SHA-256 over canonical JSON with sorted keys and no whitespace."""
    canonical = json.dumps(scene, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def write_scene(scene: dict[str, Any], path: str | Path) -> Path:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(scene, indent=1, ensure_ascii=False), encoding="utf-8")
    return path


def load_scene(path: str | Path) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def build_invalid_scene(kind: str, seed: int = 208) -> dict[str, Any]:
    """Produce deterministic broken scenes used by the validation tests."""

    scene = build_scene(seed)
    facet = scene["quadrants"][0]["facets"][0]
    if kind == "index":
        facet["triangles"][0] = [0, 3, 99]
    elif kind == "degenerate":
        facet["triangles"][0] = [0, 0, 1]
    elif kind == "transmittance":
        facet["transmittance"] = 1.7
    elif kind == "reflectance":
        facet["glaze_reflectance"] = -0.1
    elif kind == "duplicate_quadrant":
        scene["quadrants"][1]["quadrant"] = 0
    elif kind == "duplicate_facet":
        scene["quadrants"][2]["facets"][3]["facet"] = 1
    else:
        raise ValueError(f"unknown invalid scene kind: {kind}")
    return scene


def clone_scene(scene: dict[str, Any]) -> dict[str, Any]:
    return copy.deepcopy(scene)
