from __future__ import annotations

import copy
import math
from typing import Any

import numpy as np

from .util import canonical_json_bytes, sha256_bytes, stable_round


def _f(value: float) -> float:
    return stable_round(value, 7)


def _vec(values: np.ndarray) -> list[float]:
    return [_f(float(v)) for v in values]


def _canopy_layer(
    *,
    layer_index: int,
    base_y: float,
    phase: float,
    transmittance: float,
    scatter_color: list[float],
    thickness: float,
    pattern_seed: int,
) -> dict[str, Any]:
    x_values = np.linspace(-1.70, 1.70, 15)
    z_breaks = np.linspace(1.45, 4.70, 7)
    folds: list[dict[str, Any]] = []

    for fold_index in range(len(z_breaks) - 1):
        z0 = float(z_breaks[fold_index])
        z1 = float(z_breaks[fold_index + 1])
        z_values = np.linspace(z0, z1, 4)
        vertices: list[list[float]] = []
        uvs: list[list[float]] = []
        for z in z_values:
            for x in x_values:
                y = (
                    base_y
                    + 0.092 * math.sin(math.tau * (x / 0.72 + phase))
                    + 0.046
                    * math.sin(math.tau * (z / 1.16 + phase * 0.37 + 0.11))
                    + 0.021
                    * math.sin(
                        math.tau
                        * ((x * 1.43 + z * 0.86) / 0.63 + phase * 0.73)
                    )
                )
                vertices.append([_f(x), _f(y), _f(z)])
                uvs.append(
                    [
                        _f((x + 1.70) / 3.40),
                        _f((z - 1.45) / 3.25),
                    ]
                )

        nx = len(x_values)
        triangles: list[list[int]] = []
        for row in range(len(z_values) - 1):
            for col in range(nx - 1):
                a = row * nx + col
                b = a + 1
                c = a + nx
                d = c + 1
                triangles.append([a, c, b])
                triangles.append([b, c, d])

        folds.append(
            {
                "fold_id": layer_index * 100 + fold_index,
                "vertices": vertices,
                "uvs": uvs,
                "triangles": triangles,
            }
        )

    return {
        "layer_index": layer_index,
        "thickness": thickness,
        "transmittance": transmittance,
        "scatter_color": scatter_color,
        "pattern_seed": pattern_seed,
        "folds": folds,
    }


def generate_scene(
    width: int = 192,
    height: int = 160,
    tile_width: int = 64,
    tile_height: int = 40,
) -> dict[str, Any]:
    """Generate the deterministic validation scene used by the test suite."""
    layers = [
        _canopy_layer(
            layer_index=1,
            base_y=1.355,
            phase=0.08,
            transmittance=0.38,
            scatter_color=[0.82, 0.27, 0.16],
            thickness=0.032,
            pattern_seed=1103,
        ),
        _canopy_layer(
            layer_index=2,
            base_y=1.255,
            phase=0.41,
            transmittance=0.31,
            scatter_color=[0.22, 0.62, 0.45],
            thickness=0.029,
            pattern_seed=2207,
        ),
        _canopy_layer(
            layer_index=3,
            base_y=1.155,
            phase=0.73,
            transmittance=0.45,
            scatter_color=[0.88, 0.55, 0.18],
            thickness=0.036,
            pattern_seed=3313,
        ),
    ]

    depth_groups = [
        (-0.95, -0.20, 3.72, "far"),
        (-0.66, -0.22, 4.20, "mid"),
        (-0.27, -0.08, 4.68, "near"),
    ]
    tassels: list[dict[str, Any]] = []
    for z, x, name in (
        (depth_groups[0][2], depth_groups[0][0], "far_left"),
        (depth_groups[0][2], depth_groups[0][1], "far_right"),
        (depth_groups[1][2], depth_groups[1][0], "mid_left"),
        (depth_groups[1][2], depth_groups[1][1], "mid_right"),
        (depth_groups[2][2], depth_groups[2][0], "near_left"),
        (depth_groups[2][2], depth_groups[2][1], "near_right"),
    ):
        top_y = 1.05 - 0.018 * (z - 3.72)
        bottom_y = 0.44
        tassels.append(
            {
                "tassel_id": f"tassel_{name}",
                "segments": [
                    {
                        "a": [_f(x), _f(top_y), _f(z)],
                        "b": [_f(x + 0.035), _f(bottom_y), _f(z + 0.045)],
                        "radius": 0.052,
                    }
                ],
            }
        )

    return {
        "schema_version": "1.0",
        "scene_name": "cave_canopy_validation",
        "render": {
            "width": int(width),
            "height": int(height),
            "tile_width": int(tile_width),
            "tile_height": int(tile_height),
            "background_rgba": [0, 0, 0, 0],
            "tone_map_exposure": 0.92,
            "tone_map_white_cap": 0.965,
            "owner_alpha_threshold": 0.025,
        },
        "camera": {
            "position": [0.0, -1.75, -0.65],
            "target": [0.0, 0.05, 3.20],
            "up": [0.0, 1.0, 0.0],
            "fov_y_degrees": 55.0,
            "near": 0.05,
            "far": 30.0,
        },
        "wall": {
            "origin": [0.0, 0.8, 5.2],
            "normal": [0.0, 0.0, -1.0],
            "u_axis": [1.0, 0.0, 0.0],
            "v_axis": [0.0, 1.0, 0.0],
            "half_u": 2.8,
            "half_v": 2.6,
            "base_color": [0.46, 0.40, 0.31],
        },
        "fabric_shading": {
            "fold_normal_strength": 0.34,
            "min_cosine": 0.22,
            "alpha_floor": 0.34,
            "alpha_transmission_weight": 0.75,
            "ambient_strength": 0.16,
            "transmission_reference_thickness": 0.03,
        },
        "layers": layers,
        "tassels": tassels,
        "lights": [
            {
                "light_id": "lamp_left",
                "position": [-1.05, 1.63, 2.64],
                "intensity": 2.55,
                "radius": 0.15,
                "color": [1.0, 0.72, 0.42],
                "cast_shadow": True,
            },
            {
                "light_id": "lamp_center",
                "position": [0.0, 1.63, 2.86],
                "intensity": 3.15,
                "radius": 0.18,
                "color": [1.0, 0.77, 0.49],
                "cast_shadow": True,
            },
            {
                "light_id": "lamp_right",
                "position": [1.07, 1.60, 2.69],
                "intensity": 2.35,
                "radius": 0.13,
                "color": [1.0, 0.69, 0.37],
                "cast_shadow": True,
            },
        ],
        "shadows": {
            "samples_per_light": 9,
            "sample_pattern": "vogel_disk_seeded_v1",
            "edge_scan_spacing_px": 1.0,
            "edge_scan_max_px": 96,
            "measurements": [
                {"tassel_id": "tassel_far_left", "light_id": "lamp_center"},
                {"tassel_id": "tassel_mid_left", "light_id": "lamp_center"},
                {"tassel_id": "tassel_near_left", "light_id": "lamp_center"},
            ],
        },
        "validation": {
            "geometry_epsilon": 0.00001,
            "lamp_surface_clearance": 0.00001,
        },
    }


def canonical_scene(scene: dict[str, Any]) -> dict[str, Any]:
    """Normalize list order that is semantically irrelevant."""
    normalized = copy.deepcopy(scene)
    layers = normalized.get("layers")
    if isinstance(layers, list):
        for layer in layers:
            folds = layer.get("folds") if isinstance(layer, dict) else None
            if isinstance(folds, list):
                for fold in folds:
                    triangles = fold.get("triangles") if isinstance(fold, dict) else None
                    if isinstance(triangles, list):
                        triangles.sort(key=lambda t: tuple(t) if isinstance(t, list) else ())
                folds.sort(
                    key=lambda f: f.get("fold_id", -1)
                    if isinstance(f, dict)
                    else -1
                )
        layers.sort(
            key=lambda layer: layer.get("layer_index", -1)
            if isinstance(layer, dict)
            else -1
        )
    tassels = normalized.get("tassels")
    if isinstance(tassels, list):
        tassels.sort(
            key=lambda t: str(t.get("tassel_id", ""))
            if isinstance(t, dict)
            else ""
        )
    lights = normalized.get("lights")
    if isinstance(lights, list):
        lights.sort(
            key=lambda light: str(light.get("light_id", ""))
            if isinstance(light, dict)
            else ""
        )
    return normalized


def scene_digest(scene: dict[str, Any]) -> str:
    return sha256_bytes(canonical_json_bytes(canonical_scene(scene)))

