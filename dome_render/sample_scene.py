"""Deterministic local scene generator used by the demo and tests."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any

import numpy as np


def _normalized(values: list[float]) -> list[float]:
    vector = np.asarray(values, dtype=np.float64)
    return [float(value) for value in vector / np.linalg.norm(vector)]


def _board_mesh(z: float) -> dict[str, Any]:
    x0, x1 = -3.4, 3.4
    y0, y1 = -2.35, 2.65
    vertices = [
        [x0, y0, z],
        [x0, y1, z],
        [x1, y1, z],
        [x1, y0, z],
    ]
    uvs = [[0.0, 0.0], [0.0, 1.0], [1.0, 1.0], [1.0, 0.0]]
    return {"vertices": vertices, "triangles": [[0, 1, 2], [0, 2, 3]], "uvs": uvs}


def _arch_mesh(z: float, radius: float, *, segments: int = 12, width: float = 0.085, depth: float = 0.045) -> dict[str, Any]:
    center_x, center_y = 0.0, -1.05
    angles = np.linspace(0.06 * math.pi, 0.94 * math.pi, segments + 1)
    vertices: list[list[float]] = []
    for angle in angles:
        radial = np.array([math.cos(angle), math.sin(angle), 0.0])
        center = np.array([center_x + radius * math.cos(angle), center_y + radius * math.sin(angle), z])
        vertices.extend(
            [
                (center + radial * (width * 0.5) + np.array([0.0, 0.0, depth * 0.5])).tolist(),
                (center - radial * (width * 0.5) + np.array([0.0, 0.0, depth * 0.5])).tolist(),
                (center - radial * (width * 0.5) - np.array([0.0, 0.0, depth * 0.5])).tolist(),
                (center + radial * (width * 0.5) - np.array([0.0, 0.0, depth * 0.5])).tolist(),
            ]
        )
    triangles: list[list[int]] = []
    for segment in range(segments):
        a = segment * 4
        b = (segment + 1) * 4
        triangles.extend(
            [
                [a + 0, a + 1, b + 1],
                [a + 0, b + 1, b + 0],
                [a + 3, b + 3, b + 2],
                [a + 3, b + 2, a + 2],
                [a + 0, b + 0, b + 3],
                [a + 0, b + 3, a + 3],
                [a + 1, a + 2, b + 2],
                [a + 1, b + 2, b + 1],
            ]
        )
    start = 0
    triangles.extend([[start + 0, start + 3, start + 2], [start + 0, start + 2, start + 1]])
    end = segments * 4
    triangles.extend([[end + 0, end + 1, end + 2], [end + 0, end + 2, end + 3]])
    return {"vertices": vertices, "triangles": triangles}


def build_sample_scene(
    *,
    width: int = 320,
    height: int = 200,
    block_size: tuple[int, int] = (64, 40),
    supersample: int = 2,
    shadow_samples: int = 16,
    projection_density: int = 2,
    layer_gap: float = 0.45,
    light_source_distance: float = 5.5,
    light_directions: list[list[float]] | None = None,
) -> dict[str, Any]:
    layer_count = 3
    layers: list[dict[str, Any]] = []
    base_z = 3.15
    if light_directions is None:
        light_directions = [
            _normalized([0.36, 0.22, 0.91]),
            _normalized([-0.42, 0.31, 0.86]),
            _normalized([0.11, -0.47, 0.88]),
        ]
    irradiances = [
        [1.08, 0.91, 0.72],
        [0.67, 0.82, 1.06],
        [0.86, 0.77, 0.58],
    ]
    for index in range(layer_count):
        z = base_z + layer_gap * index
        ribs = []
        for rib_id, radius in enumerate((1.58, 1.98, 2.38)):
            ribs.append(
                {
                    "id": rib_id,
                    "mesh": _arch_mesh(z, radius),
                    "material": {
                        "reflectance": [0.58, 0.62, 0.66][rib_id],
                        "anisotropy": [0.57, 0.43, 0.31][rib_id],
                        "roughness": [0.23, 0.29, 0.35][rib_id],
                        "base_color": [1.0, 0.73, 0.29],
                    },
                }
            )
        layers.append(
            {
                "index": index,
                "ribs": ribs,
                "star_boards": [
                    {
                        "id": 0,
                        "mesh": _board_mesh(z),
                        "thickness": 0.36,
                        "material": {
                            "transmission": [0.80, 0.74, 0.68][index],
                            "texture_seed": 10007 + index * 7919,
                            "resolution": [192, 128],
                            "tint": [0.80 + 0.04 * index, 0.88, 1.0 - 0.03 * index],
                        },
                    }
                ],
            }
        )
    return {
        "schema_version": 1,
        "renderer": {
            "width": width,
            "height": height,
            "block_size": [block_size[0], block_size[1]],
            "sampling": {
                "supersample": supersample,
                "shadow_samples": shadow_samples,
                "projection_density": projection_density,
            },
            "exposure": 1.12,
            "background": [0.0, 0.0, 0.0, 0.0],
        },
        "camera": {
            "position": [0.0, -0.28, -5.8],
            "target": [0.0, 0.0, 3.3],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 58.0,
            "near": 0.05,
            "far": 40.0,
        },
        "lights": [
            {
                "id": index,
                "direction": light_directions[index],
                "irradiance": irradiances[index],
                "angular_radius_deg": 7.5,
                "source_distance": light_source_distance,
                "distance_softness": 0.003,
            }
            for index in range(3)
        ],
        "dome": {
            "inner_wall": {
                "plane": {"point": [0.0, 0.0, 5.35], "normal": [0.0, 0.0, -1.0]},
                "u_axis": [1.0, 0.0, 0.0],
                "v_axis": [0.0, 1.0, 0.0],
                "half_size": [4.4, 3.2],
                "resolution": [240, 180],
                "color": [0.016, 0.026, 0.052],
                "emission": [0.010, 0.016, 0.038],
                "shadow_floor": 0.28,
            },
            "layers": layers,
        },
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate a deterministic layered-dome scene JSON.")
    parser.add_argument("--out", required=True, help="Output scene path.")
    parser.add_argument("--width", type=int, default=320)
    parser.add_argument("--height", type=int, default=200)
    parser.add_argument("--block-width", type=int, default=64)
    parser.add_argument("--block-height", type=int, default=40)
    parser.add_argument("--supersample", type=int, default=2)
    parser.add_argument("--shadow-samples", type=int, default=16)
    parser.add_argument("--projection-density", type=int, default=2)
    args = parser.parse_args(argv)
    scene = build_sample_scene(
        width=args.width,
        height=args.height,
        block_size=(args.block_width, args.block_height),
        supersample=args.supersample,
        shadow_samples=args.shadow_samples,
        projection_density=args.projection_density,
    )
    path = Path(args.out)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(scene, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
