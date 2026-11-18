"""Deterministic sample scene construction.

All geometry and cutout masks are produced from integer seeds and constants;
no external models, images or network resources are used.
"""

from __future__ import annotations

import copy

from .texture import cutout_triangle_mask

# Inner (candle side) -> outer (camera side).
LAYER_SPECS = [
    # index, nominal transmittance, scatter color, seed
    (0, 0.34, (0.95, 0.10, 0.08), 1101),
    (1, 0.42, (0.98, 0.45, 0.10), 2202),
    (2, 0.50, (0.97, 0.85, 0.18), 3303),
    (3, 0.58, (0.25, 0.80, 0.28), 4404),
    (4, 0.66, (0.20, 0.45, 0.95), 5505),
]

# Geometric plane spacing: larger index sits closer to the camera.
PLANE_SPACING = 0.12
LIGHT_Z = -0.72


def _tunnel_cells(nx: int, ny: int) -> list:
    """A centered, layer-independent open column through the whole stack."""

    x0 = nx // 2 - 3
    y0 = ny // 2 - 3
    cells = []
    for cx in range(x0, x0 + 6):
        for cy in range(y0, y0 + 6):
            cells.append(cy * nx + cx)
    return cells


def _grid_mesh(nx: int, ny: int, z: float, seed: int,
                blob_count: int = 3, hole_fraction: float = 0.16,
                tunnel_cells: list | None = None) -> dict:
    tunnels = tunnel_cells if tunnel_cells is not None else _tunnel_cells(nx, ny)
    mask = cutout_triangle_mask(nx, ny, seed, blob_count, hole_fraction,
                                tunnel_cells=tunnels)
    return {
        "kind": "planar_grid",
        "grid": {
            "nx": nx, "ny": ny,
            "xmin": -1.0, "xmax": 1.0,
            "ymin": -1.0, "ymax": 1.0,
            "z": z,
        },
        "holes": [int(v) for v in mask.tolist()],
        "blob_count": blob_count,
        "hole_fraction": hole_fraction,
        "tunnel_cells": tunnels,
    }


def build_sample_scene(width: int = 320, height: int = 320,
                       tile: int = 64, sample_density: int = 9,
                       nx: int = 24, ny: int = 24,
                       gap_scale: float = 1.0) -> dict:
    layers = []
    for index, trans, color, seed in LAYER_SPECS:
        z = index * PLANE_SPACING
        layers.append({
            "index": index,
            "seed": seed,
            "mesh": _grid_mesh(nx, ny, z, seed),
            "transmittance": trans,
            "scatter_color": list(color),
            "air_gap": round(PLANE_SPACING * float(gap_scale), 6),
        })

    return {
        "schema_version": 1,
        "image": {"width": width, "height": height,
                  "background": [0.02, 0.015, 0.03, 1.0]},
        "camera": {
            "position": [0.0, 0.0, 1.35],
            "target": [0.0, 0.0, 0.24],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 45.0,
        },
        "light": {
            # Offset from the optical axis so the projection shifts visibly
            # as the inter-layer air gap changes.
            "position": [-0.06, 0.04, LIGHT_Z],
            "intensity": 2.2,
            "radius": 0.12,
        },
        "render": {
            "tile_width": tile,
            "tile_height": tile,
            "sample_density": sample_density,
            "ambient": 0.10,
        },
        "layers": layers,
    }


def build_gap_variant(scene: dict, gap_scale: float) -> dict:
    """Clone a scene with uniformly scaled air gaps and plane spacing."""

    variant = copy.deepcopy(scene)
    base = scene["layers"][0]["air_gap"]
    for layer in variant["layers"]:
        layer["air_gap"] = round(base * float(gap_scale), 6)
        mesh = layer["mesh"]
        if "grid" in mesh:
            idx = layer["index"]
            mesh["grid"]["z"] = round(idx * PLANE_SPACING * float(gap_scale), 6)
    return variant
