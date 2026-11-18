from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .util import atomic_write_bytes, canonical_json


def _mesh_for_layer(layer_index: int, layer_count: int) -> dict[str, Any]:
    nx = 12
    ny = 8
    width = 1.12 + 0.20 * layer_index
    height = 0.78 + 0.13 * layer_index
    center_x = 0.035 * layer_index - 0.06
    center_y = -0.025 + 0.012 * layer_index
    z = 0.14 + (0.88 - 0.14) * (layer_index / max(1, layer_count - 1))
    vertices: list[list[float]] = []
    uvs: list[list[float]] = []
    for iy in range(ny + 1):
        v = iy / ny
        for ix in range(nx + 1):
            u = ix / nx
            vertices.append(
                [
                    center_x + (u - 0.5) * width,
                    center_y + (v - 0.5) * height,
                    z,
                ]
            )
            uvs.append([u, v])
    triangles: list[list[int]] = []
    for iy in range(ny):
        for ix in range(nx):
            lower_left = iy * (nx + 1) + ix
            lower_right = lower_left + 1
            upper_left = lower_left + nx + 1
            upper_right = upper_left + 1
            triangles.append([lower_left, lower_right, upper_right])
            triangles.append([lower_left, upper_right, upper_left])
    return {"vertices": vertices, "triangles": triangles, "uvs": uvs}


def generate_scene(
    seed: int = 206,
    width: int = 192,
    height: int = 144,
    tile_width: int = 32,
    tile_height: int = 24,
    layer_count: int = 5,
    shadow_samples: int = 16,
    softness: float = 0.045,
) -> dict[str, Any]:
    if layer_count < 1:
        raise ValueError("layer_count must be at least 1")
    layers: list[dict[str, Any]] = []
    inner_tints = (
        [0.96, 0.70, 0.42],
        [0.91, 0.68, 0.44],
        [0.86, 0.66, 0.47],
        [0.80, 0.66, 0.51],
        [0.74, 0.66, 0.56],
    )
    transmittance = (0.46, 0.52, 0.59, 0.67, 0.75)
    for layer_index in range(layer_count):
        color = inner_tints[min(layer_index, len(inner_tints) - 1)]
        layers.append(
            {
                "layer_index": layer_index,
                "transmittance": transmittance[min(layer_index, len(transmittance) - 1)],
                "scatter_color": color,
                "texture_resolution": 96 - min(layer_index, 5) * 3,
                "mesh": _mesh_for_layer(layer_index, layer_count),
            }
        )
    return {
        "schema_version": 1,
        "seed": int(seed),
        "image": {
            "width": int(width),
            "height": int(height),
            "tile_width": int(tile_width),
            "tile_height": int(tile_height),
            "background": [0.012, 0.010, 0.009],
        },
        "camera": {
            "position": [0.0, 0.0, 2.65],
            "target": [0.0, 0.0, 0.54],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 40.0,
            "near": 0.02,
            "far": 8.0,
        },
        "light": {
            "position": [0.24, 0.11, -0.58],
            "color": [1.0, 0.78, 0.52],
            "intensity": 3.0,
        },
        "shadow": {
            "samples": int(shadow_samples),
            "softness": float(softness),
            "probe_light_distances": [0.75, 1.25, 1.75, 2.25, 3.0],
        },
        "layers": layers,
    }


def write_scene(scene: dict[str, Any], path: str | Path) -> str:
    payload = json.dumps(
        scene,
        indent=2,
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")
    payload += b"\n"
    atomic_write_bytes(Path(path), payload)
    from .util import sha256_bytes

    return sha256_bytes(canonical_json(scene))

