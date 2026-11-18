"""Deterministic sample scene generator."""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any


def _annular_sector_mesh(
    inner_radius: float,
    outer_radius: float,
    start_angle: float,
    end_angle: float,
    arc_steps: int = 2,
) -> dict[str, Any]:
    angles = [
        start_angle + (end_angle - start_angle) * index / arc_steps
        for index in range(arc_steps + 1)
    ]
    boundary: list[list[float]] = []
    for angle in angles:
        boundary.append(
            [inner_radius * math.cos(angle), inner_radius * math.sin(angle), 0.0]
        )
    for angle in reversed(angles):
        boundary.append(
            [outer_radius * math.cos(angle), outer_radius * math.sin(angle), 0.0]
        )
    center_radius = (inner_radius + outer_radius) * 0.5
    middle_angle = (start_angle + end_angle) * 0.5
    center = [
        center_radius * math.cos(middle_angle),
        center_radius * math.sin(middle_angle),
        0.0,
    ]
    vertices = [center] + boundary
    triangles = []
    boundary_count = len(boundary)
    for index in range(boundary_count):
        triangles.append([0, 1 + index, 1 + ((index + 1) % boundary_count)])
    return {"vertices": vertices, "triangles": triangles}


def build_sample_scene() -> dict[str, Any]:
    colors = {
        0: [0.075, 0.205, 0.930],
        1: [0.935, 0.055, 0.050],
        2: [0.930, 0.565, 0.085],
        3: [0.055, 0.690, 0.290],
    }
    pieces: list[dict[str, Any]] = []
    rings = [(0.18, 0.53), (0.53, 0.94)]
    angular_segments = 3
    for quadrant in range(4):
        piece_index = 0
        for ring_index, (inner_radius, outer_radius) in enumerate(rings):
            for segment in range(angular_segments):
                start_angle = math.radians(
                    quadrant * 90.0 + segment * (90.0 / angular_segments)
                )
                end_angle = math.radians(
                    quadrant * 90.0 + (segment + 1) * (90.0 / angular_segments)
                )
                transmittance = 0.67 + 0.075 * ring_index + 0.012 * segment
                lead_depth = 0.024 + 0.011 * segment + 0.006 * ring_index
                reflectance = 0.038 + 0.011 * ((piece_index + quadrant) % 3)
                pieces.append(
                    {
                        "quadrant": quadrant,
                        "piece_index": piece_index,
                        "mesh": _annular_sector_mesh(
                            inner_radius,
                            outer_radius,
                            start_angle,
                            end_angle,
                            arc_steps=2,
                        ),
                        "transmission_color": list(colors[quadrant]),
                        "transmittance": round(transmittance, 6),
                        "lead_width": 0.018,
                        "lead_depth": round(lead_depth, 6),
                        "enamel_reflectance": round(reflectance, 6),
                    }
                )
                piece_index += 1

    return {
        "schema_version": 1,
        "name": "rose-window-validation-01",
        "seed": 20261118,
        "quadrants": [0, 1, 2, 3],
        "image": {
            "width": 640,
            "height": 400,
            "chunk_size": 32,
            "gamma": 2.2,
        },
        "camera": {
            "position": [2.40, -0.55, 2.00],
            "target": [0.00, 0.05, 2.00],
            "up": [0.0, 1.0, 0.0],
            "vertical_fov_degrees": 92.0,
        },
        "window": {
            "center": [0.0, 0.0, 0.0],
            "normal": [0.0, 0.0, 1.0],
            "surround_outer_radius": 1.68,
        },
        "wall": {
            "z": 4.0,
            "width": 4.3,
            "height": 4.2,
            "center_xy": [-0.35, 0.0],
            "block_size": [0.46, 0.28],
            "color": [0.315, 0.295, 0.262],
        },
        "light": {
            "direction": [0.34, -0.16, 1.0],
            "color": [1.0, 0.955, 0.885],
            "intensity": 1.78,
            "angular_radius": 0.022,
        },
        "reflection": {
            "environment_color": [0.15, 0.215, 0.175],
            "intensity": 0.58,
            "fresnel_power": 5.0,
        },
        "texture": {
            "size": 64,
            "transmission_low": 0.80,
            "transmission_high": 1.14,
            "reflection_low": 0.76,
            "reflection_high": 1.24,
        },
        "shadow": {
            "resolution": 640,
            "samples": 18,
            "penumbra_depth_scale": 1.35,
            "profile_depth_levels": 4,
            "profile_span": 0.42,
        },
        "render": {
            "ambient_transmission": 0.215,
            "exposure": 1.16,
            "wall_ambient": 0.15,
            "wall_direct": 0.62,
        },
        "glass_pieces": pieces,
    }


def sample_scene_json() -> str:
    return json.dumps(build_sample_scene(), indent=2, sort_keys=True) + "\n"


def write_sample_scene(path: str | Path) -> None:
    Path(path).write_text(sample_scene_json(), encoding="utf-8")


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Write the deterministic sample scene")
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    write_sample_scene(args.output)
