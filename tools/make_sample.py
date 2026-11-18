#!/usr/bin/env python3
"""Build the deterministic coffered-ceiling sample scene without external assets."""

from __future__ import annotations

import argparse
import copy
import json
import math
from pathlib import Path


def quad_mesh(points, uvs):
    vertices = [[float(p[0]), float(p[1]), float(p[2])] for p in points]
    triangles = []
    for indices, tri_uv in (
        ((0, 1, 2), (uvs[0], uvs[1], uvs[2])),
        ((0, 2, 3), (uvs[0], uvs[2], uvs[3])),
    ):
        triangles.append(
            {
                "indices": list(indices),
                "uv": [[float(x), float(y)] for x, y in tri_uv],
            }
        )
    return {"vertices": vertices, "triangles": triangles}


def box_mesh(cx, cy, cz, sx, sy, sz):
    hx, hy, hz = sx * 0.5, sy * 0.5, sz * 0.5
    vertices = [
        [cx - hx, cy - hy, cz - hz],
        [cx + hx, cy - hy, cz - hz],
        [cx + hx, cy + hy, cz - hz],
        [cx - hx, cy + hy, cz - hz],
        [cx - hx, cy - hy, cz + hz],
        [cx + hx, cy - hy, cz + hz],
        [cx + hx, cy + hy, cz + hz],
        [cx - hx, cy + hy, cz + hz],
    ]
    faces = [
        (0, 2, 1),
        (0, 3, 2),
        (4, 5, 6),
        (4, 6, 7),
        (0, 1, 5),
        (0, 5, 4),
        (1, 2, 6),
        (1, 6, 5),
        (2, 3, 7),
        (2, 7, 6),
        (3, 0, 4),
        (3, 4, 7),
    ]
    triangles = []
    for index, face in enumerate(faces):
        triangles.append(
            {
                "indices": list(face),
                "uv": [
                    [0.0, 0.0],
                    [1.0, 0.0],
                    [0.0, 1.0],
                ],
            }
        )
    return {"vertices": vertices, "triangles": triangles}


def ring_boards(layer_index, z, inner, outer, color, seeds):
    mid = (inner + outer) * 0.5
    definitions = [
        (
            "N",
            [
                (-outer, inner, z),
                (outer, inner, z),
                (outer, outer, z),
                (-outer, outer, z),
            ],
            0.25,
            0.75,
        ),
        (
            "E",
            [
                (inner, -inner, z),
                (outer, -inner, z),
                (outer, inner, z),
                (inner, inner, z),
            ],
            0.25,
            0.75,
        ),
        (
            "S",
            [
                (-outer, -outer, z),
                (outer, -outer, z),
                (outer, -inner, z),
                (-outer, -inner, z),
            ],
            0.25,
            0.75,
        ),
        (
            "W",
            [
                (-outer, -inner, z),
                (-inner, -inner, z),
                (-inner, inner, z),
                (-outer, inner, z),
            ],
            0.25,
            0.75,
        ),
    ]
    boards = []
    for name, points, u0, v0 in definitions:
        uv = [(u0, v0), (1.0 - u0, v0), (1.0 - u0, 1.0 - v0), (u0, 1.0 - v0)]
        boards.append(
            {
                "id": f"B{layer_index}{name}",
                "seed": seeds[name],
                "transmittance": 0.48 + layer_index * 0.055,
                "color": color,
                "mesh": quad_mesh(points, uv),
            }
        )
    return boards


def layer_brackets(layer_index, z, thickness, mid, color):
    brackets = []
    positions = [
        ("N", 0.0, mid, 0.26, 0.075),
        ("E", mid, 0.0, 0.075, 0.26),
        ("S", 0.0, -mid, 0.26, 0.075),
        ("W", -mid, 0.0, 0.075, 0.26),
    ]
    for idx, (name, cx, cy, sx, sy) in enumerate(positions):
        brackets.append(
            {
                "id": f"K{layer_index}{name}",
                "reflectance": 0.34 + layer_index * 0.055 + idx * 0.012,
                "thickness": thickness,
                "color": color,
                "mesh": box_mesh(cx, cy, z, sx, sy, thickness),
            }
        )
    return brackets


def make_scene() -> dict:
    radii = [(0.22, 0.52), (0.42, 0.82), (0.72, 1.12)]
    board_seeds = [
        {"N": 1101, "E": 1102, "S": 1103, "W": 1104},
        {"N": 2201, "E": 2202, "S": 2203, "W": 2204},
        {"N": 3301, "E": 3302, "S": 3303, "W": 3304},
    ]
    layers = []
    for index, (inner, outer) in enumerate(radii):
        z = 0.04 + index * 0.255
        thickness = 0.040 + index * 0.008
        mid = (inner + outer) * 0.5
        board_color = [0.76 - index * 0.04, 0.43 - index * 0.015, 0.17 + index * 0.02]
        bracket_color = [0.20 + index * 0.035, 0.25 + index * 0.02, 0.23]
        layers.append(
            {
                "layer": index,
                "z": z,
                "thickness": thickness,
                "boards": ring_boards(
                    index, z, inner, outer, board_color, board_seeds[index]
                ),
                "brackets": layer_brackets(
                    index, z, thickness, mid, bracket_color
                ),
            }
        )
    return {
        "schema": "coffered-ceiling-soft-raster/v1",
        "name": "deterministic-three-layer-coffer",
        "render": {
            "width": 192,
            "height": 128,
            "tile_size": 32,
            "texture_resolution": 64,
            "projection_samples": 3,
            "shadow_map_resolution": 256,
            "penumbra_scale": 0.012,
            "exposure": 0.82,
            "ambient": 0.14,
            "background": [0.002, 0.003, 0.004],
        },
        "layers": layers,
        "lights": [
            {
                "id": "L_WARM",
                "position": [-0.52, -0.34, -0.56],
                "color": [1.0, 0.79, 0.57],
                "intensity": 1.35,
            },
            {
                "id": "L_COOL",
                "position": [0.46, 0.31, -0.52],
                "color": [0.60, 0.77, 1.0],
                "intensity": 1.10,
            },
            {
                "id": "L_BACK",
                "position": [0.04, 0.62, -0.48],
                "color": [1.0, 0.90, 0.73],
                "intensity": 0.82,
            },
        ],
        "receiver": {
            "z": 1.24,
            "min_x": -1.48,
            "max_x": 1.48,
            "min_y": -1.08,
            "max_y": 1.08,
            "color": [0.29, 0.31, 0.30],
        },
        "camera": {
            "position": [0.0, 0.0, -1.82],
            "target": [0.0, 0.0, 0.34],
            "up": [0.0, 1.0, 0.0],
            "fov_deg": 58.0,
            "near": 0.02,
            "far": 12.0,
        },
    }


def apply_variant(scene: dict, variant: str) -> dict:
    result = copy.deepcopy(scene)
    if variant == "index_oob":
        result["layers"][0]["boards"][0]["mesh"]["triangles"][0]["indices"] = [999, 1, 2]
    elif variant == "degenerate":
        result["layers"][0]["boards"][0]["mesh"]["triangles"][0]["indices"] = [0, 0, 1]
    elif variant == "transmittance":
        result["layers"][0]["boards"][0]["transmittance"] = 1.25
    elif variant == "reflectance":
        result["layers"][1]["brackets"][0]["reflectance"] = -0.1
    elif variant == "duplicate_id":
        result["layers"][2]["boards"][1]["id"] = result["layers"][2]["boards"][0]["id"]
    elif variant == "missing_id":
        result["layers"][0]["brackets"][2]["id"] = ""
    elif variant == "light_on_plane":
        result["lights"][1]["position"][2] = result["layers"][1]["z"]
    elif variant == "bad_light_color":
        result["lights"][0]["color"] = [1.4, 0.2, 0.1]
    elif variant == "bad_camera":
        result["camera"]["fov_deg"] = 0.0
    elif variant == "triangle_order":
        for layer in result["layers"]:
            for component in layer["boards"] + layer["brackets"]:
                component["mesh"]["triangles"].reverse()
    elif variant == "seed_change":
        result["layers"][1]["boards"][0]["seed"] += 1
    elif variant == "light_shift":
        result["lights"][0]["position"][0] += 0.12
        result["lights"][0]["position"][1] -= 0.08
    elif variant == "clean":
        pass
    else:
        raise SystemExit(f"unknown variant: {variant}")
    return result


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument(
        "--variant",
        default="clean",
        choices=[
            "clean",
            "index_oob",
            "degenerate",
            "transmittance",
            "reflectance",
            "duplicate_id",
            "missing_id",
            "light_on_plane",
            "bad_light_color",
            "bad_camera",
            "triangle_order",
            "seed_change",
            "light_shift",
        ],
    )
    args = parser.parse_args()
    scene = apply_variant(make_scene(), args.variant)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(scene, indent=2, sort_keys=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
