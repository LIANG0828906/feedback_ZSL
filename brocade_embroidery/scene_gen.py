"""Deterministic construction of the embroidered-stage scene description.

The generator only consumes fixed integer seeds and geometric parameters; no
external models, textures or network resources are involved.
"""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .rng import uniform_float

DEFAULT_WIDTH = 1920
DEFAULT_HEIGHT = 1080


def _undulation(x: np.ndarray, y: np.ndarray, layer_index: int,
                amplitude: float) -> np.ndarray:
    phase = 0.7 * layer_index
    return amplitude * (
        np.sin(1.15 * x + phase) * np.cos(0.85 * y - 0.4 * phase)
        + 0.4 * np.sin(0.55 * x - 1.3 * y + phase)
    )


def _build_layer(layer_index: int, kind: str, z_plane: float,
                 cols: int, rows: int, seed: int) -> dict[str, Any]:
    width = 10.0
    height = 7.0
    center_y = 0.4
    amplitude = 0.16 if kind == "satin" else 0.05
    x_coords = np.linspace(-width / 2.0, width / 2.0, cols + 1)
    y_coords = np.linspace(center_y - height / 2.0,
                           center_y + height / 2.0, rows + 1)
    grid_x, grid_y = np.meshgrid(x_coords, y_coords, indexing="xy")
    grid_z = z_plane + _undulation(grid_x, grid_y, layer_index, amplitude)
    vertices = np.stack([grid_x, grid_y, grid_z], axis=-1).reshape(-1, 3)

    triangles: list[list[int]] = []
    triangle_components: list[str] = []
    component_a = f"{kind.upper()[0]}{layer_index}-A"
    component_b = f"{kind.upper()[0]}{layer_index}-B"
    for row in range(rows):
        for col in range(cols):
            p00 = row * (cols + 1) + col
            p10 = p00 + 1
            p01 = p00 + cols + 1
            p11 = p01 + 1
            # Fixed winding; traversal order is part of the fixed seed data.
            triangles.append([p00, p10, p11])
            triangles.append([p00, p11, p01])
            component = component_a if col < cols // 2 else component_b
            triangle_components.extend([component, component])

    base_colors = [
        [0.48, 0.09, 0.13],
        [0.30, 0.16, 0.34],
        [0.52, 0.14, 0.10],
        [0.20, 0.24, 0.38],
        [0.58, 0.20, 0.12],
    ]
    layer: dict[str, Any] = {
        "layer_index": layer_index,
        "kind": kind,
        "component_ids": [component_a, component_b],
        "plane": {
            "point": [0.0, center_y, float(z_plane)],
            "normal": [0.0, 0.0, 1.0],
        },
        "width": width,
        "height": height,
        "cols": cols,
        "rows": rows,
        "vertices": np.round(vertices, 9).tolist(),
        "triangles": triangles,
        "triangle_components": triangle_components,
        "air_gap_to_previous": 0.0 if layer_index == 0 else 0.8,
    }
    if kind == "satin":
        jitter = float(uniform_float(seed, layer_index, 11))
        layer["material"] = {
            "base_color": base_colors[layer_index],
            # Anisotropic reflectivity is constrained to the valid (0, 1) band.
            "anisotropic_reflectivity": float(0.34 + 0.24 * jitter),
            "roughness": float(0.22 + 0.18 * uniform_float(seed,
                                                           layer_index, 13)),
            "embroidery_seed": int(seed + 1000 + layer_index),
            "table_resolution": 128,
            "thread_direction_deg": float(10.0 + 14.0 * (layer_index % 3)),
        }
    else:
        layer["material"] = {
            "base_color": [0.72, 0.66, 0.52],
            "transmittance": float(
                0.42 + 0.16 * uniform_float(seed, layer_index, 17)),
            "hole_seed": int(seed + 2000 + layer_index),
            "hole_table_resolution": 128,
            "hole_rate": 0.32,
        }
    return layer


def generate_scene(*, small: bool = False, seed: int = 20260101) -> dict[str, Any]:
    """Build the full scene description with fixed deterministic parameters."""
    cols, rows = (18, 12) if small else (56, 32)
    z_planes = [-2.0, -1.2, -0.4, 0.4, 1.2]
    kinds = ["satin", "kesi", "satin", "kesi", "satin"]
    layers = [
        _build_layer(index, kinds[index], z_planes[index], cols, rows, seed)
        for index in range(5)
    ]
    if small:
        tile_width, tile_height = 160, 90
        width, height = 320, 180
        penumbra_samples = 8
    else:
        tile_width, tile_height = 240, 135
        width, height = DEFAULT_WIDTH, DEFAULT_HEIGHT
        penumbra_samples = 16
    scene: dict[str, Any] = {
        "schema_version": 1,
        "deterministic_seed": seed,
        "frame": {
            "width": width,
            "height": height,
            "fov_y_deg": 60.0,
            "tile_width": tile_width,
            "tile_height": tile_height,
            "penumbra_samples": penumbra_samples,
        },
        "camera": {
            "target": [0.0, 0.3, -0.4],
            "radius": 12.0,
            "up": [0.0, 1.0, 0.0],
            "elevation_deg": 6.0,
            "orbit_azimuth_deg": [-35.0, -15.0, 0.0, 15.0, 35.0],
        },
        "floor": {
            "point": [0.0, -3.2, 0.0],
            "normal": [0.0, 1.0, 0.0],
            "color": [0.16, 0.10, 0.07],
            "extent_x": 13.0,
            "extent_z": 9.0,
        },
        "lights": [
            {
                "id": "L0",
                "position": [-3.6, 2.4, -4.8],
                "intensity": 1.15,
                "color": [1.0, 0.86, 0.70],
            },
            {
                "id": "L1",
                "position": [3.9, 1.1, -5.6],
                "intensity": 0.95,
                "color": [0.72, 0.82, 1.0],
            },
        ],
        "layers": layers,
    }
    return scene


def scene_canonical_json(scene: dict[str, Any]) -> bytes:
    """Canonical byte serialization used for digest and snapshot validation."""
    import json

    return json.dumps(
        scene, sort_keys=True, separators=(",", ":"),
        ensure_ascii=False, allow_nan=False,
    ).encode("utf-8")


def scene_digest(scene: dict[str, Any]) -> str:
    import hashlib

    return hashlib.sha256(scene_canonical_json(scene)).hexdigest()


def embroidery_seed_signature(scene: dict[str, Any]) -> str:
    import hashlib

    parts = []
    for layer in scene["layers"]:
        material = layer["material"]
        if layer["kind"] == "satin":
            parts.append(f"{layer['layer_index']}:satin:"
                         f"{material['embroidery_seed']}")
        else:
            parts.append(f"{layer['layer_index']}:kesi:"
                         f"{material['hole_seed']}")
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()


def expected_tile_count(scene: dict[str, Any]) -> tuple[int, int, int]:
    frame = scene["frame"]
    columns = math.ceil(frame["width"] / frame["tile_width"])
    rows = math.ceil(frame["height"] / frame["tile_height"])
    return columns, rows, columns * rows
