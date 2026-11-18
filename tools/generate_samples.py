#!/usr/bin/env python3
"""Generate deterministic caisson scenes and business-error fixtures."""

from __future__ import annotations

import argparse
import copy
import json
import math
from pathlib import Path


def polar_quadrant(
    quadrant: int,
    y: float,
    inner_radius: float,
    outer_radius: float,
    columns: int,
    rows: int,
    angle_offset_degrees: float,
) -> dict:
    theta0 = math.radians(quadrant * 90.0 + angle_offset_degrees)
    theta1 = theta0 + math.pi / 2.0
    vertices = []
    uvs = []
    indices = []
    for row in range(rows + 1):
        radius = inner_radius + (outer_radius - inner_radius) * row / rows
        for column in range(columns + 1):
            t = column / columns
            theta = theta0 + (theta1 - theta0) * t
            vertices.append(
                [
                    round(radius * math.cos(theta), 9),
                    round(y, 9),
                    round(radius * math.sin(theta), 9),
                ]
            )
            uvs.append([round(t, 9), round(row / rows, 9)])
    for row in range(rows):
        for column in range(columns):
            a = row * (columns + 1) + column
            b = (row + 1) * (columns + 1) + column
            c = a + 1
            d = b + 1
            indices.extend([a, b, c, b, d, c])
    return {
        "quadrant": quadrant,
        "grid": {"columns": columns, "rows": rows},
        "vertices": vertices,
        "uvs": uvs,
        "indices": indices,
    }


def box_mesh(center: list[float], size: list[float]) -> dict:
    cx, cy, cz = center
    hx, hy, hz = (value * 0.5 for value in size)
    vertices = [
        [cx - hx, cy - hy, cz - hz],
        [cx + hx, cy - hy, cz - hz],
        [cx + hx, cy - hy, cz + hz],
        [cx - hx, cy - hy, cz + hz],
        [cx - hx, cy + hy, cz - hz],
        [cx + hx, cy + hy, cz - hz],
        [cx + hx, cy + hy, cz + hz],
        [cx - hx, cy + hy, cz + hz],
    ]
    faces = [
        (0, 3, 2, 1),
        (4, 5, 6, 7),
        (0, 1, 5, 4),
        (1, 2, 6, 5),
        (2, 3, 7, 6),
        (3, 0, 4, 7),
    ]
    indices = []
    for a, b, c, d in faces:
        indices.extend([a, b, c, a, c, d])
    return {
        "grid": {"columns": 2, "rows": 2},
        "vertices": [[round(value, 9) for value in vertex] for vertex in vertices],
        "indices": indices,
    }


def layer(
    layer_id: int,
    y: float,
    color: list[float],
    transmittance: float,
    reflectance: float,
    angle_offset: float,
) -> dict:
    return {
        "layer_id": layer_id,
        "wood_color": color,
        "transmittance": transmittance,
        "reflectance": reflectance,
        "lattice_width": 0.060,
        "quadrants": [
            polar_quadrant(
                quadrant,
                y,
                0.20,
                1.50,
                columns=7,
                rows=4,
                angle_offset_degrees=angle_offset,
            )
            for quadrant in range(4)
        ],
    }


def main_scene(seed: int = 20261118) -> dict:
    layers = [
        layer(1, 3.12, [0.88, 0.30, 0.15], 0.60, 0.10, -7.0),
        layer(2, 3.40, [0.18, 0.63, 0.54], 0.48, 0.15, 4.0),
        layer(3, 3.68, [0.96, 0.70, 0.22], 0.36, 0.20, 11.0),
    ]
    return {
        "schema_version": 1,
        "seed": seed,
        "image": {
            "width": 480,
            "height": 360,
            "tile_size": 40,
            "aa_samples": 2,
            "background": [0.018, 0.013, 0.010],
        },
        "camera": {
            "position": [0.16, 1.06, 3.92],
            "target": [0.0, 3.42, 0.0],
            "up": [0.0, 1.0, 0.0],
            "vertical_fov_degrees": 56.0,
            "near": 0.08,
            "far": 30.0,
        },
        "ambient": [0.040, 0.029, 0.019],
        "layers": layers,
        "lamps": [
            {
                "id": "lamp_a",
                "position": [-0.56, 1.62, -0.34],
                "color": [1.0, 0.61, 0.28],
                "intensity": 6.4,
                "radius": 0.12,
            },
            {
                "id": "lamp_b",
                "position": [0.54, 1.66, 0.31],
                "color": [1.0, 0.77, 0.47],
                "intensity": 5.8,
                "radius": 0.13,
            },
            {
                "id": "lamp_c",
                "position": [0.02, 1.58, 0.86],
                "color": [1.0, 0.45, 0.20],
                "intensity": 5.2,
                "radius": 0.11,
            },
        ],
        "dougong": [
            {
                "id": "dougong_deep",
                "color": [0.24, 0.12, 0.055],
                **box_mesh([-0.48, 2.10, -0.14], [0.62, 0.20, 0.24]),
            },
            {
                "id": "dougong_shallow",
                "color": [0.22, 0.11, 0.050],
                **box_mesh([0.50, 2.72, 0.22], [0.62, 0.17, 0.24]),
            },
        ],
        "shadow": {
            "samples_per_axis": 6,
            "probe_samples": 65,
            "probe_half_span": 2.4,
        },
    }


def write_json(path: Path, value: dict) -> None:
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False) + "\n",
        encoding="utf-8",
    )


def generate_scene(seed: int = 20261118) -> dict:
    return main_scene(seed)


def generate_samples(output_dir: Path, seed: int = 20261118) -> None:
    output_dir.mkdir(parents=True, exist_ok=True)
    scene = main_scene(seed)
    write_json(output_dir / "scene.json", scene)

    reordered = copy.deepcopy(scene)
    reordered["layers"] = list(reversed(reordered["layers"]))
    for layer_value in reordered["layers"]:
        layer_value["quadrants"] = list(reversed(layer_value["quadrants"]))
    reordered["lamps"] = list(reversed(reordered["lamps"]))
    reordered["dougong"] = list(reversed(reordered["dougong"]))
    write_json(output_dir / "scene_reordered.json", reordered)

    invalid_index = copy.deepcopy(scene)
    invalid_index["layers"][0]["quadrants"][0]["indices"][0] = 999999
    write_json(output_dir / "invalid_index.json", invalid_index)

    invalid_degenerate = copy.deepcopy(scene)
    invalid_degenerate["layers"][0]["quadrants"][0]["indices"][1] = invalid_degenerate[
        "layers"
    ][0]["quadrants"][0]["indices"][0]
    invalid_degenerate["layers"][0]["quadrants"][0]["indices"][2] = invalid_degenerate[
        "layers"
    ][0]["quadrants"][0]["indices"][0]
    write_json(output_dir / "invalid_degenerate.json", invalid_degenerate)

    invalid_transmittance = copy.deepcopy(scene)
    invalid_transmittance["layers"][0]["transmittance"] = 1.25
    write_json(output_dir / "invalid_transmittance.json", invalid_transmittance)

    invalid_reflectance = copy.deepcopy(scene)
    invalid_reflectance["layers"][1]["reflectance"] = -0.2
    write_json(output_dir / "invalid_reflectance.json", invalid_reflectance)

    duplicate_layer = copy.deepcopy(scene)
    duplicate_layer["layers"][1]["layer_id"] = duplicate_layer["layers"][0]["layer_id"]
    write_json(output_dir / "invalid_duplicate_layer.json", duplicate_layer)

    lamp_inside = copy.deepcopy(scene)
    lamp_inside["lamps"][0]["position"] = copy.deepcopy(
        lamp_inside["layers"][0]["quadrants"][0]["vertices"][0]
    )
    write_json(output_dir / "invalid_lamp_inside.json", lamp_inside)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", type=Path, default=Path("samples"))
    parser.add_argument("--seed", type=int, default=20261118)
    args = parser.parse_args()
    generate_samples(args.output_dir, args.seed)


if __name__ == "__main__":
    main()

