"""Deterministic closed dish meshes and sample scene construction."""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

import numpy as np


def dish_mesh(
    radius: float,
    thickness: float,
    dish_depth: float,
    z_center: float,
    radial_segments: int = 32,
    rings: int = 8,
) -> tuple[np.ndarray, np.ndarray]:
    """Build a closed, watertight, shallow dish layer around a local center plane."""

    if radial_segments < 8 or rings < 2:
        raise ValueError("dish mesh needs at least 8 segments and 2 rings")
    angles = np.arange(radial_segments, dtype=np.float64) * (2.0 * np.pi / radial_segments)
    radii = radius * np.arange(1, rings + 1, dtype=np.float64) / rings
    z_top = z_center + thickness * 0.5 + dish_depth * np.square(radii / radius)
    z_bottom = z_center - thickness * 0.5 + dish_depth * np.square(radii / radius)

    vertices: list[list[float]] = []
    top_center = len(vertices)
    vertices.append([0.0, 0.0, float(z_center + thickness * 0.5)])
    top_rings: list[list[int]] = []
    for ring_index, (radius_value, z_value) in enumerate(zip(radii, z_top)):
        ring: list[int] = []
        for angle in angles:
            ring.append(len(vertices))
            vertices.append(
                [
                    float(radius_value * math.cos(angle)),
                    float(radius_value * math.sin(angle)),
                    float(z_value),
                ]
            )
        top_rings.append(ring)

    bottom_center = len(vertices)
    vertices.append([0.0, 0.0, float(z_center - thickness * 0.5)])
    bottom_rings: list[list[int]] = []
    for radius_value, z_value in zip(radii, z_bottom):
        ring = []
        for angle in angles:
            ring.append(len(vertices))
            vertices.append(
                [
                    float(radius_value * math.cos(angle)),
                    float(radius_value * math.sin(angle)),
                    float(z_value),
                ]
            )
        bottom_rings.append(ring)

    triangles: list[list[int]] = []
    for segment in range(radial_segments):
        nxt = (segment + 1) % radial_segments
        triangles.append([top_center, top_rings[0][segment], top_rings[0][nxt]])
        triangles.append([bottom_center, bottom_rings[0][nxt], bottom_rings[0][segment]])
    for ring_index in range(rings - 1):
        inner = top_rings[ring_index]
        outer = top_rings[ring_index + 1]
        binner = bottom_rings[ring_index]
        bouter = bottom_rings[ring_index + 1]
        for segment in range(radial_segments):
            nxt = (segment + 1) % radial_segments
            triangles.append([inner[segment], outer[segment], outer[nxt]])
            triangles.append([inner[segment], outer[nxt], inner[nxt]])
            triangles.append([binner[segment], bouter[nxt], bouter[segment]])
            triangles.append([binner[segment], binner[nxt], bouter[nxt]])
    top_rim = top_rings[-1]
    bottom_rim = bottom_rings[-1]
    for segment in range(radial_segments):
        nxt = (segment + 1) % radial_segments
        triangles.append([top_rim[segment], bottom_rim[segment], bottom_rim[nxt]])
        triangles.append([top_rim[segment], bottom_rim[nxt], top_rim[nxt]])
    return (
        np.asarray(vertices, dtype=np.float64),
        np.asarray(triangles, dtype=np.int64),
    )


def quaternion_from_yaw(yaw_rad: float) -> list[float]:
    return [float(math.cos(yaw_rad * 0.5)), 0.0, 0.0, float(math.sin(yaw_rad * 0.5))]


def make_scene(
    width: int = 384,
    height: int = 256,
    tile_size: int = 64,
    plate_gap: float = 0.19,
    base_seed: int = 20261118,
    radial_segments: int = 32,
    rings: int = 8,
    texture_resolution: int = 128,
    projection_samples: int = 12,
) -> dict[str, Any]:
    """Construct the fully deterministic reference scene."""

    plate_count = 3
    plates: list[dict[str, Any]] = []
    layers: list[dict[str, Any]] = []
    glaze_color = [0.36, 0.63, 0.57]
    body_color = [0.22, 0.39, 0.36]

    for plate_index in range(plate_count):
        yaw = math.radians((plate_index - 1) * 5.5)
        plates.append(
            {
                "plate_id": plate_index,
                "center": [0.0, 0.0, plate_index * plate_gap],
                "orientation": quaternion_from_yaw(yaw),
                "radius": 0.80,
            }
        )
        layer_specs = [
            {
                "layer_id": 0,
                "kind": "body",
                "transmittance": 0.13,
                "scatter_color": body_color,
                "radius": 0.76,
                "thickness": 0.040,
                "dish_depth": 0.075,
                "z_center": -0.022,
            },
            {
                "layer_id": 1,
                "kind": "glaze",
                "transmittance": 0.63,
                "scatter_color": glaze_color,
                "radius": 0.80,
                "thickness": 0.006,
                "dish_depth": 0.082,
                "z_center": -0.002,
            },
            {
                "layer_id": 2,
                "kind": "glaze",
                "transmittance": 0.72,
                "scatter_color": glaze_color,
                "radius": 0.80,
                "thickness": 0.005,
                "dish_depth": 0.082,
                "z_center": 0.004,
            },
        ]
        for spec in layer_specs:
            vertices, triangles = dish_mesh(
                radius=spec["radius"],
                thickness=spec["thickness"],
                dish_depth=spec["dish_depth"],
                z_center=spec["z_center"],
                radial_segments=radial_segments,
                rings=rings,
            )
            seed = base_seed + plate_index * 1009 + spec["layer_id"] * 137
            layers.append(
                {
                    "plate_id": plate_index,
                    "layer_id": spec["layer_id"],
                    "kind": spec["kind"],
                    "seed": seed,
                    "transmittance": spec["transmittance"],
                    "scatter_color": spec["scatter_color"],
                    "texture_resolution": [texture_resolution, texture_resolution],
                    "carve_depth": 1.0,
                    "vertices": vertices.tolist(),
                    "triangles": triangles.tolist(),
                }
            )

    stack_mid_z = (plate_count - 1) * plate_gap * 0.5
    target = [0.0, 0.0, stack_mid_z + 0.015]
    return {
        "schema": "celadon-scene-v1",
        "scene_name": "three-plate-celadon-stack",
        "image": {
            "width": width,
            "height": height,
            "tile_size": tile_size,
            "background": [0.011, 0.017, 0.021],
        },
        "camera": {
            "position": [1.95, -2.55, 2.05],
            "target": target,
            "up": [0.0, 0.0, 1.0],
            "vfov_deg": 35.0,
            "near": 0.02,
            "far": 50.0,
        },
        "lights": [
            {
                "id": "warm-key",
                "position": [-1.45, -0.95, 4.10],
                "intensity": 5.6,
                "color": [1.0, 0.92, 0.78],
                "radius": 0.20,
            },
            {
                "id": "cool-fill",
                "position": [1.55, 1.10, 3.05],
                "intensity": 3.1,
                "color": [0.78, 0.90, 1.0],
                "radius": 0.27,
            },
        ],
        "plates": plates,
        "layers": layers,
        "render": {
            "tile_size": tile_size,
            "ambient": 0.31,
            "exposure": 0.94,
            "max_display": 0.94,
            "projection_strength": 0.62,
            "soft_shadow_samples": projection_samples,
            "projection_sample_density": projection_samples,
            "projection_threshold": 0.57,
            "depth_scale": 8.0,
        },
        "stack": {
            "plate_gap": plate_gap,
            "spacer_ring_height": plate_gap,
        },
    }


def write_scene(path: str | Path, document: dict[str, Any]) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(document, ensure_ascii=False, indent=2, sort_keys=True, allow_nan=False)
    destination.write_text(text + "\n", encoding="utf-8")
