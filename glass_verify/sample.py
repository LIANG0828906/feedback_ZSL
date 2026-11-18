from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any


def _quad(x0: float, x1: float, y0: float, y1: float, z: float) -> dict[str, Any]:
    return {
        "vertices": [
            [x0, y0, z],
            [x1, y0, z],
            [x1, y1, z],
            [x0, y1, z],
        ],
        "triangles": [[0, 1, 2], [0, 2, 3]],
    }


def _triangle(
    points: list[list[float]],
) -> dict[str, Any]:
    return {
        "vertices": points,
        "triangles": [[0, 1, 2]],
    }


def make_sample_scene(seed: int = 20261118) -> dict[str, Any]:
    """Build the deterministic local acceptance scene.

    The window is a stylised pointed arch split into three vertical sectors.
    Sectors 0/1/2 are blue/ruby/emerald. A small diamond in sector 0 overlaps
    the base panes at a nearer depth to exercise order-independent layering.
    """
    phase = (seed % 997) / 997.0
    sector_specs = [
        {
            "sector_id": 0,
            "x0": -0.62,
            "x1": 0.62,
            "z": 0.060,
            "color": [0.035, 0.20, 0.98],
            "transmittance": 0.74,
            "reflectance": 0.035,
            "lead_depth": 0.040,
        },
        {
            "sector_id": 1,
            "x0": -1.86,
            "x1": -0.62,
            "z": 0.025,
            "color": [0.98, 0.055, 0.075],
            "transmittance": 0.70,
            "reflectance": 0.050,
            "lead_depth": 0.085,
        },
        {
            "sector_id": 2,
            "x0": 0.62,
            "x1": 1.86,
            "z": -0.010,
            "color": [0.035, 0.78, 0.24],
            "transmittance": 0.66,
            "reflectance": 0.060,
            "lead_depth": 0.140,
        },
    ]
    row_edges = [0.35, 1.14, 1.94, 2.66]
    apexes = [3.78, 3.50, 3.50]
    panes: list[dict[str, Any]] = []
    for spec in sector_specs:
        sector_id = int(spec["sector_id"])
        for pane_id in range(3):
            y0 = row_edges[pane_id]
            y1 = row_edges[pane_id + 1]
            shape = _quad(float(spec["x0"]), float(spec["x1"]), y0, y1, float(spec["z"]))
            panes.append(
                {
                    "sector_id": sector_id,
                    "pane_id": pane_id,
                    "transmission_color": list(spec["color"]),
                    "transmittance": float(spec["transmittance"]),
                    "glaze_reflectance": float(spec["reflectance"]) + pane_id * 0.006,
                    "lead_width": 0.030 + pane_id * 0.003,
                    "lead_depth": float(spec["lead_depth"]) * (0.86 + 0.14 * pane_id),
                    **shape,
                }
            )
        mid = 0.5 * (float(spec["x0"]) + float(spec["x1"]))
        panes.append(
            {
                "sector_id": sector_id,
                "pane_id": 3,
                "transmission_color": list(spec["color"]),
                "transmittance": max(0.52, float(spec["transmittance"]) - 0.11),
                "glaze_reflectance": float(spec["reflectance"]) + 0.012,
                "lead_width": 0.034,
                "lead_depth": float(spec["lead_depth"]) * 1.40,
                **_triangle(
                    [
                        [float(spec["x0"]), row_edges[-1], float(spec["z"])],
                        [float(spec["x1"]), row_edges[-1], float(spec["z"])],
                        [mid, apexes[sector_id], float(spec["z"])],
                    ]
                ),
            }
        )
    # A medial diamond, nearer than the blue base panes. It makes multiple
    # glass intersections possible without relying on input list order.
    panes.append(
        {
            "sector_id": 0,
            "pane_id": 4,
            "transmission_color": [0.075, 0.32, 0.90],
            "transmittance": 0.56,
            "glaze_reflectance": 0.045,
            "lead_width": 0.026,
            "lead_depth": 0.030,
            **_quad(-0.27, 0.27, 1.72, 2.24, 0.086),
        }
    )

    direction = [0.55 + 0.04 * phase, -0.22, 0.80]
    length = math.sqrt(sum(component * component for component in direction))
    direction = [component / length for component in direction]
    return {
        "schema_version": 1,
        "sectors": [
            {"sector_id": 0},
            {"sector_id": 1},
            {"sector_id": 2},
        ],
        "window": {
            "origin": [0.0, 0.0, 0.0],
            "normal": [0.0, 0.0, 1.0],
            "axis_u": [1.0, 0.0, 0.0],
            "axis_v": [0.0, 1.0, 0.0],
        },
        "camera": {
            "position": [-1.15, 1.02, 4.80],
            "target": [0.12, 1.98, 0.0],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 58.0,
            "width": 180,
            "height": 120,
            "near": 0.08,
            "far": 20.0,
        },
        "directional_light": {
            "direction": direction,
            "irradiance": [3.00, 2.85, 2.70],
            "bands_nm": [650.0, 550.0, 450.0],
            "angular_radius_deg": 1.25,
        },
        "texture": {
            "seed": int(seed),
            "table_size": 40,
            "transmission_strength": 0.28,
            "reflection_strength": 0.50,
            "octaves": 3,
        },
        "panes": panes,
        "walls": [
            {
                "id": "side_aisle",
                "point": [2.75, 2.00, 2.30],
                "normal": [-1.0, 0.0, 0.0],
                "axis_u": [0.0, 0.0, 1.0],
                "axis_v": [0.0, 1.0, 0.0],
                "extent_u": 6.60,
                "extent_v": 4.20,
                "albedo": [0.36, 0.335, 0.295],
                "receives_shadow": True,
            },
            {
                "id": "window_surround",
                "point": [0.0, 0.0, -0.035],
                "normal": [0.0, 0.0, 1.0],
                "axis_u": [1.0, 0.0, 0.0],
                "axis_v": [0.0, 1.0, 0.0],
                "extent_u": 6.20,
                "extent_v": 4.40,
                "albedo": [0.30, 0.285, 0.255],
                "receives_shadow": False,
            },
        ],
        "render": {
            "chunk_size": [45, 60],
            "supersample": 2,
            "exposure": 0.42,
            "background": [0.008, 0.010, 0.014],
            "ambient_light": [0.050, 0.056, 0.068],
            "reflection_tint": [0.78, 0.84, 0.94],
            "composite_order": "transmission_then_reflection",
            "grazing_reflectance_cap": 0.28,
            "output_rounding": "half_up",
        },
        "projection": {
            "wall_index": 0,
            "samples_per_axis": 9,
            "penumbra_per_depth": 0.55,
            "profile_points": 33,
            "depth_levels": 5,
        },
    }


def write_sample_scene(path: str | Path, seed: int = 20261118) -> dict[str, Any]:
    scene = make_sample_scene(seed)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(scene, ensure_ascii=False, indent=2, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    return scene
