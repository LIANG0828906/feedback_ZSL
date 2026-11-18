from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .util import atomic_write_text


def _ellipse(x: np.ndarray, y: np.ndarray, cx: float, cy: float, rx: float, ry: float) -> np.ndarray:
    return ((x - cx) / rx) ** 2 + ((y - cy) / ry) ** 2 <= 1.0


def _segment_distance(
    x: np.ndarray,
    y: np.ndarray,
    ax: float,
    ay: float,
    bx: float,
    by: float,
) -> np.ndarray:
    dx = bx - ax
    dy = by - ay
    denominator = dx * dx + dy * dy
    t = np.clip(((x - ax) * dx + (y - ay) * dy) / denominator, 0.0, 1.0)
    return np.sqrt((x - (ax + t * dx)) ** 2 + (y - (ay + t * dy)) ** 2)


def _puppet_mask(x: np.ndarray, y: np.ndarray) -> np.ndarray:
    head = _ellipse(x, y, 0.0, 1.04, 0.34, 0.39)
    headdress = (
        (y >= 1.30)
        & (y <= 1.55)
        & (np.abs(x) <= 0.26 * (1.55 - y) / 0.25)
    )
    body = (y >= -0.25) & (y <= 0.78) & (np.abs(x) <= 0.24 + 0.12 * (0.78 - y) / 1.03)
    left_arm = _segment_distance(x, y, -0.22, 0.58, -0.64, 0.17) <= 0.105
    right_arm = _segment_distance(x, y, 0.22, 0.58, 0.68, 0.20) <= 0.11
    left_leg = _segment_distance(x, y, -0.13, -0.20, -0.27, -0.94) <= 0.135
    right_leg = _segment_distance(x, y, 0.13, -0.20, 0.27, -0.93) <= 0.135
    left_foot = _ellipse(x, y, -0.31, -1.02, 0.17, 0.08)
    right_foot = _ellipse(x, y, 0.31, -1.01, 0.17, 0.08)
    return head | headdress | body | left_arm | right_arm | left_leg | right_leg | left_foot | right_foot


def _hole_cells(rows: int, cols: int, x_edges: np.ndarray, y_edges: np.ndarray) -> dict[int, set[tuple[int, int]]]:
    x_centers = 0.5 * (x_edges[:-1] + x_edges[1:])
    y_centers = 0.5 * (y_edges[:-1] + y_edges[1:])
    xx, yy = np.meshgrid(x_centers, y_centers)
    silhouette = _puppet_mask(xx, yy)

    definitions = {
        1: _ellipse(xx, yy, -0.14, 1.04, 0.075, 0.075),
        2: _segment_distance(xx, yy, 0.39, 0.42, 0.44, 0.33) <= 0.085,
        3: _ellipse(xx, yy, -0.24, -0.65, 0.105, 0.105),
        4: _ellipse(xx, yy, 0.02, 0.16, 0.070, 0.085),
    }
    result: dict[int, set[tuple[int, int]]] = {}
    for cutout_id, mask in definitions.items():
        cells = {
            (int(col), int(row))
            for row, col in np.argwhere(mask & silhouette)
        }
        if not cells:
            candidates = np.argwhere(silhouette)
            target_centers = {
                1: (-0.14, 1.04),
                2: (0.42, 0.37),
                3: (-0.24, -0.65),
                4: (0.02, 0.16),
            }
            tx, ty = target_centers[cutout_id]
            best = min(
                candidates,
                key=lambda pair: (
                    (x_centers[pair[1]] - tx) ** 2 + (y_centers[pair[0]] - ty) ** 2,
                    int(pair[0]),
                    int(pair[1]),
                ),
            )
            cells = {(int(best[1]), int(best[0]))}
        result[cutout_id] = cells
    return result


def _make_layer_mesh(
    layer_index: int,
    z: float,
    offset_x: float,
    offset_y: float,
    rows: int,
    cols: int,
    x_edges: np.ndarray,
    y_edges: np.ndarray,
) -> tuple[list[list[float]], list[list[int]], list[dict[str, Any]]]:
    x_centers = 0.5 * (x_edges[:-1] + x_edges[1:])
    y_centers = 0.5 * (y_edges[:-1] + y_edges[1:])
    xx, yy = np.meshgrid(x_centers, y_centers)
    silhouette = _puppet_mask(xx, yy)
    holes = _hole_cells(rows, cols, x_edges, y_edges)
    cell_cutout: dict[tuple[int, int], int] = {}
    for cutout_id, cells in holes.items():
        for cell in cells:
            if cell in cell_cutout:
                raise RuntimeError("sample cutout cells overlap")
            cell_cutout[cell] = cutout_id

    vertices = [
        [float(x_edges[col] + offset_x), float(y_edges[row] + offset_y), float(z)]
        for row in range(rows + 1)
        for col in range(cols + 1)
    ]
    triangles: list[list[int]] = []
    cutout_triangles: dict[int, list[int]] = {cutout_id: [] for cutout_id in holes}
    for row in range(rows):
        for col in range(cols):
            if not bool(silhouette[row, col]):
                continue
            v00 = row * (cols + 1) + col
            v10 = v00 + 1
            v01 = v00 + (cols + 1)
            v11 = v01 + 1
            triangle_a = len(triangles)
            triangles.append([v00, v10, v11])
            cutout_id = cell_cutout.get((col, row), 0)
            if cutout_id:
                cutout_triangles[cutout_id].append(triangle_a)
            triangle_b = len(triangles)
            triangles.append([v00, v11, v01])
            if cutout_id:
                cutout_triangles[cutout_id].append(triangle_b)

    cutouts = [
        {
            "cutout_id": cutout_id,
            "triangle_indices": cutout_triangles[cutout_id],
        }
        for cutout_id in sorted(cutout_triangles)
    ]
    if any(not entry["triangle_indices"] for entry in cutouts):
        raise RuntimeError(f"layer {layer_index} has an empty cutout")
    return vertices, triangles, cutouts


def make_sample_scene(
    *,
    width: int = 192,
    height: int = 144,
    block_width: int = 48,
    block_height: int = 36,
    supersampling: int = 2,
    projection_samples: int = 4,
) -> dict[str, Any]:
    rows = 22
    cols = 18
    x_edges = np.linspace(-1.05, 1.05, cols + 1)
    y_edges = np.linspace(-1.45, 1.45, rows + 1)
    layer_specs = [
        (-0.050, 0.000, 0.000, 0.36, [0.74, 0.29, 0.10], 104729, 0.011),
        (0.000, 0.008, -0.006, 0.49, [0.74, 0.29, 0.10], 130363, 0.014),
        (0.050, -0.006, 0.008, 0.31, [0.88, 0.54, 0.17], 155921, 0.009),
        (0.100, 0.004, 0.003, 0.58, [0.58, 0.18, 0.07], 196613, 0.017),
    ]
    layers: list[dict[str, Any]] = []
    for layer_index, (z, ox, oy, transmittance, tint, seed, thickness) in enumerate(layer_specs):
        vertices, triangles, cutouts = _make_layer_mesh(
            layer_index,
            z,
            ox,
            oy,
            rows,
            cols,
            x_edges,
            y_edges,
        )
        layers.append(
            {
                "layer_index": layer_index,
                "mesh": {
                    "vertices": vertices,
                    "triangles": triangles,
                },
                "cutouts": cutouts,
                "transmittance": transmittance,
                "tint": tint,
                "fiber_seed": seed,
                "thickness": thickness,
            }
        )

    return {
        "schema_version": 1,
        "metadata": {
            "name": "four-layer-leather-shadow-puppet",
            "description": "Deterministic validation scene generated by leather_raster.sample",
            "layer_order": "layer_index 0 is nearest the camera; higher indices approach the screen",
        },
        "render": {
            "width": width,
            "height": height,
            "block_width": block_width,
            "block_height": block_height,
            "supersampling": supersampling,
            "projection_samples": projection_samples,
            "exposure": 0.90,
            "ambient": 0.14,
            "background": [0.012, 0.009, 0.006, 1.0],
        },
        "camera": {
            "position": [0.0, -0.02, -3.80],
            "target": [0.0, -0.02, 0.12],
            "up": [0.0, 1.0, 0.0],
            "fov_degrees": 44.0,
            "near": 0.05,
            "far": 12.0,
        },
        "screen": {
            "origin": [-1.90, -1.65, 0.95],
            "u_axis": [1.0, 0.0, 0.0],
            "v_axis": [0.0, 1.0, 0.0],
            "width": 3.80,
            "height": 3.30,
            "tint": [0.56, 0.42, 0.25],
            "fiber_seed": 8675309,
        },
        "lights": [
            {
                "id": "lamp_left",
                "position": [-1.42, -0.48, -1.46],
                "intensity": 2.10,
                "color": [1.00, 0.66, 0.30],
                "radius": 0.11,
                "softness_gain": 1.0,
            },
            {
                "id": "lamp_center",
                "position": [0.12, 0.82, -1.92],
                "intensity": 1.72,
                "color": [1.00, 0.82, 0.47],
                "radius": 0.11,
                "softness_gain": 1.0,
            },
            {
                "id": "lamp_right",
                "position": [1.38, -0.10, -1.58],
                "intensity": 2.34,
                "color": [0.94, 0.55, 0.22],
                "radius": 0.11,
                "softness_gain": 1.0,
            },
        ],
        "layers": layers,
    }


def write_sample_scene(path: str | Path, **overrides: Any) -> dict[str, Any]:
    scene = make_sample_scene(**overrides)
    atomic_write_text(
        path,
        json.dumps(scene, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
    )
    return scene

