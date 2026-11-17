#!/usr/bin/env python3
"""Generate deterministic local church-floor mosaic scenes."""

from __future__ import annotations

import argparse
import json
import random
from pathlib import Path

import numpy as np


def box_floor(half_extent: float, thickness: float) -> dict:
    x = half_extent
    z = -thickness
    vertices = [
        [-x, 0.0, -x],
        [x, 0.0, -x],
        [x, 0.0, x],
        [-x, 0.0, x],
        [-x, z, -x],
        [x, z, -x],
        [x, z, x],
        [-x, z, x],
    ]
    triangles = [
        [0, 2, 1],
        [0, 3, 2],
        [4, 5, 6],
        [4, 6, 7],
        [0, 1, 5],
        [0, 5, 4],
        [1, 2, 6],
        [1, 6, 5],
        [2, 3, 7],
        [2, 7, 6],
        [3, 0, 4],
        [3, 4, 7],
    ]
    return {"vertices": vertices, "triangles": triangles}


def tile_vertices(center_x: float, center_z: float, half_size: float, height: float) -> list:
    return [
        [center_x - half_size, height, center_z - half_size],
        [center_x + half_size, height, center_z - half_size],
        [center_x + half_size, height, center_z + half_size],
        [center_x - half_size, height, center_z + half_size],
    ]


def make_scene() -> dict:
    rng = np.random.default_rng(232)
    layer_transmission = [
        [0.54, 0.69, 0.76],
        [0.63, 0.54, 0.72],
        [0.69, 0.67, 0.39],
    ]
    layer_ior = [1.46, 1.54, 1.62]
    layer_height = [0.075, 0.155, 0.235]
    layer_thickness = [0.014, 0.020, 0.027]
    centers = [-0.92, 0.0, 0.92]
    pieces = []
    piece_id = 0
    for layer in range(3):
        for zi, center_z in enumerate(centers):
            for xi, center_x in enumerate(centers):
                # Deterministic gaps in the outermost layer exercise floor fallback.
                if layer == 2 and (xi == 2 and zi == 0):
                    piece_id += 1
                    continue
                jitter = float(rng.uniform(-0.0012, 0.0012))
                thickness = layer_thickness[layer] + jitter
                vertices = tile_vertices(center_x, center_z, 0.415, layer_height[layer])
                pieces.append(
                    {
                        "layerIndex": layer,
                        "pieceId": piece_id,
                        "transmission": layer_transmission[layer],
                        "thickness": round(thickness, 9),
                        "edgeIor": layer_ior[layer],
                        "vertices": vertices,
                        "triangles": [[0, 1, 2], [0, 2, 3]],
                    }
                )
                piece_id += 1

    return {
        "format": "mosaic-scene-v1",
        "seed": 232,
        "camera": {
            "position": [0.0, 3.35, 3.65],
            "target": [0.0, 0.14, 0.0],
            "up": [0.0, 1.0, 0.0],
            "fovYDeg": 44.0,
            "near": 0.01,
            "far": 30.0,
        },
        "floor": box_floor(2.15, 0.16),
        "pieces": pieces,
        "lights": [
            {
                "id": 0,
                "position": [-1.55, 3.08, 1.25],
                "color": [1.0, 0.84, 0.61],
                "intensity": 2.15,
            },
            {
                "id": 1,
                "position": [1.78, 2.72, 0.82],
                "color": [0.58, 0.79, 1.0],
                "intensity": 1.72,
            },
            {
                "id": 2,
                "position": [-0.22, 2.36, -1.82],
                "color": [1.0, 0.94, 0.83],
                "intensity": 1.36,
            },
        ],
        "settings": {
            "image": {
                "width": 320,
                "height": 240,
                "chunkWidth": 64,
                "chunkHeight": 48,
            },
            "texture": {"bubblesPerPiece": 10, "flowBandsPerPiece": 3},
            "caustic": {
                "samplesPerEdge": 14,
                "penumbraBase": 0.0045,
                "penumbraThickness": 0.62,
                "penumbraLayerDistance": 0.105,
                "intensity": 0.23,
                "edgeBand": 0.12,
                "splatSupport": 2.8,
            },
            "glare": {"reflectionScale": 0.68, "edgePower": 4.5},
            "compositionOrder": ["transmission", "reflection"],
        },
        "metadata": {
            "title": "deterministic church floor mosaic validation scene",
            "generator": "python/generate_scene.py",
        },
    }


def shuffled_copy(scene: dict) -> dict:
    shuffled = json.loads(json.dumps(scene))
    random.Random(2329).shuffle(shuffled["pieces"])
    random.Random(815).shuffle(shuffled["lights"])
    for piece in shuffled["pieces"]:
        piece["triangles"].reverse()
    return shuffled


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out-dir", default="samples")
    args = parser.parse_args()
    output = Path(args.out_dir)
    output.mkdir(parents=True, exist_ok=True)
    scene = make_scene()
    (output / "church_floor.json").write_text(
        json.dumps(scene, indent=2, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    (output / "church_floor_shuffled.json").write_text(
        json.dumps(shuffled_copy(scene), indent=2, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    print(
        f"wrote {output / 'church_floor.json'} with "
        f"{len(scene['pieces'])} pieces and {len(scene['lights'])} lights"
    )


if __name__ == "__main__":
    main()
