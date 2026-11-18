"""Deterministic construction of the gallery scene, geometry and samples."""

from __future__ import annotations

import math
from typing import Any


def _slab_geometry(
    x0: float,
    x1: float,
    y0: float,
    y1: float,
    z_center: float,
    thickness: float,
) -> tuple[list[list[float]], list[list[int]], list[list[float]]]:
    zf = z_center + thickness * 0.5
    zb = z_center - thickness * 0.5
    vertices = [
        [x0, y0, zf],
        [x1, y0, zf],
        [x1, y1, zf],
        [x0, y1, zf],
        [x0, y0, zb],
        [x1, y0, zb],
        [x1, y1, zb],
        [x0, y1, zb],
    ]
    triangles = [
        [0, 1, 2],
        [0, 2, 3],
        [5, 4, 7],
        [5, 7, 6],
        [0, 4, 5],
        [0, 5, 1],
        [1, 5, 6],
        [1, 6, 2],
        [2, 6, 7],
        [2, 7, 3],
        [3, 7, 4],
        [3, 4, 0],
    ]
    uvs = [
        [0.0, 0.0],
        [1.0, 0.0],
        [1.0, 1.0],
        [0.0, 1.0],
        [0.0, 0.0],
        [1.0, 0.0],
        [1.0, 1.0],
        [0.0, 1.0],
    ]
    return vertices, triangles, uvs


def _seed(layer: int, leaf: int) -> int:
    return 41017 + layer * 100003 + leaf * 7919 + leaf * leaf * 131


def build_scene() -> dict[str, Any]:
    """Build a complete JSON-serialisable scene without external assets or RNG."""

    columns = 4
    rows = 4
    leaf_width = 0.80
    leaf_height = 0.58
    gap_x = 0.12
    gap_y = 0.16
    total_width = columns * leaf_width + (columns - 1) * gap_x
    total_height = rows * leaf_height + (rows - 1) * gap_y
    origin_x = -total_width * 0.5
    origin_y = 1.82 - total_height * 0.5
    layer_z = [-0.30, -0.14, 0.02, 0.18]
    x_stagger = [0.0, 0.062, -0.045, 0.094]
    y_stagger = [0.0, 0.034, -0.022, 0.052]
    brush_base_deg = [-8.0, 14.0, -3.0, 21.0]

    layers: list[dict[str, Any]] = []
    leaves: list[dict[str, Any]] = []
    for layer in range(4):
        leaf_ids: list[int] = []
        for row in range(rows):
            for column in range(columns):
                leaf = row * columns + column
                leaf_ids.append(leaf)
                x0 = origin_x + column * (leaf_width + gap_x) + x_stagger[layer]
                y0 = origin_y + row * (leaf_height + gap_y) + y_stagger[layer]
                x1 = x0 + leaf_width
                y1 = y0 + leaf_height
                seed = _seed(layer, leaf)
                # Millimetre-scale thickness variation, always inside the declared bounds.
                thickness = 0.019 + ((seed >> 7) % 23) * 0.001
                angle_deg = (
                    brush_base_deg[layer]
                    + ((seed >> 11) % 29 - 14) * 0.62
                    + column * 1.7
                    - row * 0.9
                )
                angle_rad = math.radians(angle_deg)
                brush = [math.cos(angle_rad), math.sin(angle_rad), 0.0]
                vertices, triangles, uvs = _slab_geometry(
                    x0, x1, y0, y1, layer_z[layer], thickness
                )
                warm = ((seed >> 17) % 7 - 3) * 0.007
                leaves.append(
                    {
                        "layer": layer,
                        "leaf": leaf,
                        "seed": seed,
                        "thickness": round(thickness, 6),
                        "brush_direction": [round(c, 9) for c in brush],
                        "vertices": vertices,
                        "triangles": triangles,
                        "uvs": uvs,
                        "material": {
                            "albedo": [
                                round(0.595 + warm, 6),
                                round(0.607 + warm * 0.72, 6),
                                round(0.612 + warm * 0.38, 6),
                            ],
                            "roughness_along": round(
                                0.105 + ((seed >> 5) % 9) * 0.0045, 6
                            ),
                            "roughness_across": round(
                                0.31 + ((seed >> 9) % 11) * 0.008, 6
                            ),
                            "specular": round(0.56 + ((seed >> 13) % 7) * 0.012, 6),
                            "aperture_scale": 1.0,
                        },
                    }
                )
        layers.append(
            {
                "layer": layer,
                "name": ["inner", "middle-a", "middle-b", "outer"][layer],
                "z_center": layer_z[layer],
                "gap_to_outer_m": 0.16,
                "leaf_ids": leaf_ids,
            }
        )

    scene = {
        "schema_version": 1,
        "scene_id": "gallery-louver-array-v1",
        "units": "metres",
        "layers": layers,
        "leaves": leaves,
        "receivers": {
            "wall": {
                "id": "wall",
                "plane_axis": "z",
                "plane_value": -2.20,
                "normal": [0.0, 0.0, 1.0],
                "bounds": {"x": [-3.20, 3.20], "y": [0.0, 4.30]},
                "albedo": [0.57, 0.545, 0.515],
                "roughness": 0.86,
            },
            "floor": {
                "id": "floor",
                "plane_axis": "y",
                "plane_value": 0.0,
                "normal": [0.0, 1.0, 0.0],
                "bounds": {"x": [-3.20, 3.20], "z": [-2.20, 3.20]},
                "albedo": [0.48, 0.46, 0.43],
                "roughness": 0.52,
            },
        },
        "lights": [
            {
                "id": "track-L",
                "position": [-2.55, 3.34, 2.10],
                "target": [-0.82, 1.58, -0.18],
                "intensity": 132.0,
                "color": [1.0, 0.81, 0.58],
                "inner_cone_deg": 23.0,
                "outer_cone_deg": 46.0,
                "falloff": 2.0,
                "radius_m": 0.075,
            },
            {
                "id": "track-C",
                "position": [0.02, 3.64, 2.66],
                "target": [0.0, 2.02, -0.14],
                "intensity": 148.0,
                "color": [0.60, 0.82, 1.0],
                "inner_cone_deg": 20.0,
                "outer_cone_deg": 43.0,
                "falloff": 2.0,
                "radius_m": 0.070,
            },
            {
                "id": "track-R",
                "position": [2.58, 3.18, 1.92],
                "target": [0.86, 1.31, -0.20],
                "intensity": 118.0,
                "color": [1.0, 0.94, 0.80],
                "inner_cone_deg": 25.0,
                "outer_cone_deg": 48.0,
                "falloff": 2.0,
                "radius_m": 0.082,
            },
        ],
        "camera": {
            "center": [0.0, 1.92, -0.03],
            "radius": 5.30,
            "elevation_deg": 9.0,
            "angles_deg": [-52.0, -18.0, 18.0, 52.0],
            "fov_deg": 38.0,
            "near": 0.05,
            "far": 30.0,
            "width": 480,
            "height": 360,
        },
        "render": {
            "tile_width": 80,
            "tile_height": 60,
            "pattern_resolution": [128, 128],
            "microsurface_resolution": [40, 16],
            "shadow_sample_density": 5,
            "pixel_supersamples": 1,
            "ambient_strength": 0.17,
            "highlight_soft_knee": 0.42,
            "tone_map_white": 1.85,
            "thickness_bounds_m": [0.008, 0.080],
            "softness": {
                "base_m": 0.0015,
                "leaf_to_receiver_m_per_m": 0.014,
                "light_to_leaf_m_per_m": 0.0042,
            },
            "interlayer_gap_m": 0.16,
            "seed_domain": "GLV-SEED-V1",
        },
    }
    return scene
