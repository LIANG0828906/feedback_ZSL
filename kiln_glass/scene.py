"""Deterministic construction and (de)serialisation of the kiln scene.

The scene is generated entirely from integer constants in code plus the
top level scene seed: no external assets are required.
"""

from __future__ import annotations

import json
import numpy as np

from .model import (
    Brick,
    Camera,
    Layer,
    PointLight,
    ReceiverPlane,
    RenderConfig,
    Scene,
)


def _box_mesh(origin: np.ndarray, size: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Triangulated box mesh with outward winding and unique vertices."""
    ox, oy, oz = origin
    sx, sy, sz = size
    coords = np.array(
        [
            [ox, oy, oz],
            [ox + sx, oy, oz],
            [ox + sx, oy + sy, oz],
            [ox, oy + sy, oz],
            [ox, oy, oz + sz],
            [ox + sx, oy, oz + sz],
            [ox + sx, oy + sy, oz + sz],
            [ox, oy + sy, oz + sz],
        ],
        dtype=np.float64,
    )
    triangles = np.array(
        [
            [0, 3, 2], [0, 2, 1],  # back  (normal -z)
            [4, 5, 6], [4, 6, 7],  # front (normal +z)
            [0, 1, 5], [0, 5, 4],  # bottom
            [2, 3, 7], [2, 7, 6],  # top
            [0, 4, 7], [0, 7, 3],  # left
            [1, 2, 6], [1, 6, 5],  # right
        ],
        dtype=np.int64,
    )
    return coords, triangles


def build_default_scene() -> dict:
    """Construct the reference three-layer glass-brick wall description."""
    master_seed = 20261118
    layer_specs = [
        # layer, plane_z (back face), thickness, grout, cols, rows, x_offset
        {"layer": 0, "plane_z": -0.60, "thickness": 0.28, "grout": 0.05,
         "cols": 4, "rows": 3, "offset": 0.0},
        {"layer": 1, "plane_z": -0.25, "thickness": 0.22, "grout": 0.05,
         "cols": 4, "rows": 3, "offset": 0.35},
        {"layer": 2, "plane_z": 0.05, "thickness": 0.18, "grout": 0.05,
         "cols": 4, "rows": 3, "offset": 0.15},
    ]
    brick_w, brick_h = 0.70, 0.60
    pitch_x, pitch_y = brick_w + 0.05, brick_h + 0.05
    width = 4 * pitch_x
    height = 3 * pitch_y

    bricks: list[dict] = []
    layers: list[dict] = []
    brick_id = 0
    for spec in layer_specs:
        layer_ids = []
        for row in range(spec["rows"]):
            for col in range(spec["cols"]):
                x0 = -width / 2.0 + col * pitch_x + spec["offset"]
                y0 = -height / 2.0 + row * pitch_y
                origin = [x0, y0, spec["plane_z"]]
                size = [brick_w, brick_h, spec["thickness"]]
                verts, tris = _box_mesh(np.array(origin), np.array(size))
                # Deterministic per-brick material variation from position.
                transmittance = float(
                    0.72
                    + 0.12 * (((col * 7 + row * 13 + spec["layer"] * 5) % 5) / 4.0)
                )
                bricks.append(
                    {
                        "brick_id": brick_id,
                        "layer": spec["layer"],
                        "origin": origin,
                        "size": size,
                        "vertices": verts.tolist(),
                        "triangles": tris.tolist(),
                        "thickness": spec["thickness"],
                        "transmittance": round(transmittance, 6),
                        "fringe_seed": master_seed + brick_id * 101 + spec["layer"] * 7,
                    }
                )
                layer_ids.append(brick_id)
                brick_id += 1
        layers.append(
            {
                "index": spec["layer"],
                "plane_z": spec["plane_z"],
                "grout_width": spec["grout"],
                "brick_ids": layer_ids,
            }
        )

    lights = [
        {"light_id": 0, "position": [-1.25, -0.55, -2.30], "intensity": 1.05},
        {"light_id": 1, "position": [0.35, 0.70, -2.85], "intensity": 0.95},
        {"light_id": 2, "position": [1.35, -0.15, -3.45], "intensity": 0.85},
    ]
    receiver = {
        "z": 0.42,
        "x_min": -2.4,
        "x_max": 2.4,
        "y_min": -1.7,
        "y_max": 1.7,
    }
    camera = {
        "position": [0.0, 0.0, 2.7],
        "target": [0.0, 0.0, -0.35],
        "up": [0.0, 1.0, 0.0],
        "focal_length": 1.0,
        "sensor_width": 2.05,
        "sensor_height": 1.55,
    }
    view_angles = [-0.30, -0.10, 0.0, 0.12, 0.28]
    config = {
        "image_width": 96,
        "image_height": 72,
        "tile_width": 32,
        "tile_height": 24,
        "sample_density": 16,
        "bubble_count": 6,
        "background_rgb": [0.015, 0.012, 0.01],
    }
    return {
        "scene_seed": master_seed,
        "wall": {"layers": layers, "bricks": bricks},
        "lights": lights,
        "receiver_plane": receiver,
        "camera": camera,
        "view_angles": view_angles,
        "render_config": config,
    }


def write_scene(description: dict, path: str) -> None:
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(description, handle, indent=2, sort_keys=True)
        handle.write("\n")


def load_scene_dict(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def parse_scene(description: dict) -> Scene:
    """Convert a validated JSON description into typed NumPy structures."""
    bricks = []
    for entry in description["wall"]["bricks"]:
        bricks.append(
            Brick(
                layer=int(entry["layer"]),
                brick_id=int(entry["brick_id"]),
                vertices=np.asarray(entry["vertices"], dtype=np.float64),
                triangles=np.asarray(entry["triangles"], dtype=np.int64),
                thickness=float(entry["thickness"]),
                transmittance=float(entry["transmittance"]),
                fringe_seed=int(entry["fringe_seed"]),
                origin=np.asarray(entry["origin"], dtype=np.float64),
                size=np.asarray(entry["size"], dtype=np.float64),
            )
        )
    layers = tuple(
        Layer(
            index=int(entry["index"]),
            plane_z=float(entry["plane_z"]),
            grout_width=float(entry["grout_width"]),
            brick_ids=tuple(int(v) for v in entry["brick_ids"]),
        )
        for entry in description["wall"]["layers"]
    )
    lights = tuple(
        PointLight(
            light_id=int(entry["light_id"]),
            position=np.asarray(entry["position"], dtype=np.float64),
            intensity=float(entry["intensity"]),
        )
        for entry in description["lights"]
    )
    receiver_entry = description["receiver_plane"]
    receiver = ReceiverPlane(
        z=float(receiver_entry["z"]),
        x_min=float(receiver_entry["x_min"]),
        x_max=float(receiver_entry["x_max"]),
        y_min=float(receiver_entry["y_min"]),
        y_max=float(receiver_entry["y_max"]),
    )
    camera_entry = description["camera"]
    camera = Camera(
        position=np.asarray(camera_entry["position"], dtype=np.float64),
        target=np.asarray(camera_entry["target"], dtype=np.float64),
        up=np.asarray(camera_entry["up"], dtype=np.float64),
        focal_length=float(camera_entry["focal_length"]),
        sensor_width=float(camera_entry["sensor_width"]),
        sensor_height=float(camera_entry["sensor_height"]),
    )
    cfg_entry = description["render_config"]
    config = RenderConfig(
        image_width=int(cfg_entry["image_width"]),
        image_height=int(cfg_entry["image_height"]),
        tile_width=int(cfg_entry["tile_width"]),
        tile_height=int(cfg_entry["tile_height"]),
        sample_density=int(cfg_entry["sample_density"]),
        bubble_count=int(cfg_entry["bubble_count"]),
        background_rgb=tuple(float(v) for v in cfg_entry["background_rgb"]),
    )
    return Scene(
        bricks=tuple(bricks),
        layers=layers,
        lights=lights,
        receiver=receiver,
        camera=camera,
        view_angles=tuple(float(v) for v in description["view_angles"]),
        config=config,
        raw=description,
    )


def canonical_scene_bytes(description: dict) -> bytes:
    """Canonical byte representation used for snapshot/fingerprint checks."""
    return json.dumps(description, sort_keys=True, separators=(",", ":")).encode("utf-8")
