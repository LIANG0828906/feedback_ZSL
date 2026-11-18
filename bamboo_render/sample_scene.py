from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any, Sequence


def make_sample_scene(
    *,
    gaps: Sequence[float] = (0.0, 0.22, 0.26),
    image_width: int = 320,
    image_height: int = 240,
    chunk_width: int = 64,
    chunk_height: int = 48,
    panel_width: float = 2.2,
    panel_height: float = 1.5,
    divisions_x: int = 20,
    divisions_y: int = 14,
) -> dict[str, Any]:
    if len(gaps) != 3:
        raise ValueError("sample scene requires exactly three layers")
    if gaps[0] != 0.0:
        raise ValueError("innermost gap must be zero")
    z_values: list[float] = []
    z = 0.0
    for index, gap in enumerate(gaps):
        if index > 0:
            z += float(gap)
        z_values.append(z)

    layer_names = ("inner_leaf_liner", "middle_sixfold_weave", "outer_guard_weave")
    layers = []
    for index, gap in enumerate(gaps):
        layer_number = index + 1
        vertices, triangles, uvs, strand_ids = _panel_mesh(
            panel_width,
            panel_height,
            divisions_x,
            divisions_y,
            z_values[index],
            layer_number,
            weave_pitch=0.14,
        )
        layers.append(
            {
                "layer": layer_number,
                "name": layer_names[index],
                "air_gap": float(gap),
                "weave_pitch": 0.14,
                "strand_diameter": 0.024,
                "texture_resolution": [192, 192],
                "liner": {
                    "transmittance": 0.27,
                    "scatter_color": [0.76, 0.50, 0.18],
                    "reflectance": 0.18,
                    "fiber_contrast": 0.24,
                },
                "strand_color": [0.38, 0.24, 0.09],
                "mesh": {
                    "vertices": vertices,
                    "triangles": triangles,
                    "strand_ids": strand_ids,
                    "uvs": uvs,
                },
            }
        )

    return {
        "schema_version": 1,
        "scene_name": "deterministic_three_layer_bamboo_lampshade",
        "seed": 20261118,
        "layer_origin_z": 0.0,
        "image": {
            "width": image_width,
            "height": image_height,
            "chunk_size": [chunk_width, chunk_height],
        },
        "camera": {
            "position": [0.55, -0.40, -4.40],
            "target": [0.0, 0.0, 0.55],
            "up": [0.0, 1.0, 0.0],
            "fov_y_degrees": 42.0,
            "near": 0.05,
            "far": 20.0,
        },
        "light": {
            "position": [-0.32, 0.18, -1.25],
            "intensity": 2.2,
            "color": [1.0, 0.80, 0.51],
        },
        "inner_wall": {
            "point": [0.0, 0.0, 0.88],
            "normal": [0.0, 0.0, -1.0],
            "color": [0.075, 0.052, 0.036],
            "reflectance": 0.08,
        },
        "shadow": {
            "sampling_density": 9,
            "penumbra_per_gap": 0.16,
        },
        "layers": layers,
    }


def write_sample_scene(path: str | Path, **kwargs: Any) -> dict[str, Any]:
    scene = make_sample_scene(**kwargs)
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(scene, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return scene


def _panel_mesh(
    width: float,
    height: float,
    nx: int,
    ny: int,
    z: float,
    layer: int,
    weave_pitch: float,
) -> tuple[list[list[float]], list[list[int]], list[list[float]], list[int]]:
    if nx < 1 or ny < 1:
        raise ValueError("mesh divisions must be positive")
    vertices: list[list[float]] = []
    uvs: list[list[float]] = []
    for iy in range(ny + 1):
        v = iy / ny
        for ix in range(nx + 1):
            u = ix / nx
            x = (u - 0.5) * width
            y = (v - 0.5) * height
            vertices.append([round(x, 9), round(y, 9), round(z, 9)])
            uvs.append([round(u, 9), round(v, 9)])

    triangles: list[list[int]] = []
    strand_ids: list[int] = []
    stride = nx + 1
    for iy in range(ny):
        for ix in range(nx):
            v00 = iy * stride + ix
            v10 = v00 + 1
            v01 = v00 + stride
            v11 = v01 + 1
            triangles.append([v00, v10, v11])
            triangles.append([v00, v11, v01])
            for tri in (triangles[-2], triangles[-1]):
                cx = sum(vertices[index][0] for index in tri) / 3.0
                cy = sum(vertices[index][1] for index in tri) / 3.0
                strand_ids.append(_strand_id(cx, cy, weave_pitch, layer))

    return vertices, triangles, uvs, strand_ids


def _strand_id(x: float, y: float, pitch: float, layer: int) -> int:
    sqrt3 = math.sqrt(3.0)
    normals = ((1.0, 0.0), (0.5, sqrt3 / 2.0), (-0.5, sqrt3 / 2.0))
    phases = [math.cos(2.0 * math.pi * (x * nx + y * ny) / pitch) for nx, ny in normals]
    family = max(range(3), key=lambda index: phases[index])
    nx, ny = normals[family]
    cell = int(math.floor((x * nx + y * ny) / pitch + 4096.0))
    return layer * 1000000 + family * 100000 + (cell % 100000)
