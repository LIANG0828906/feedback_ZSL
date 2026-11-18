"""Camera-independent receiver light maps for art review and energy inspection."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np

from .png import encode_png_rgba
from .projection import ProjectionSystem
from .render import display_rgba8
from .scene import Scene
from .shading import RECEIVER_SPOT_GAIN, receiver_surface_color
from .util import (
    atomic_write_bytes,
    atomic_write_npy,
    object_digest,
    sha256_bytes,
)


def _receiver_grid(
    scene: Scene, receiver_id: str, width: int, height: int
) -> tuple[np.ndarray, np.ndarray]:
    receiver = scene.receivers[receiver_id]
    axes = [axis for axis in ("x", "y", "z") if axis != receiver.plane_axis]
    horizontal, vertical = axes
    h_values = np.linspace(
        receiver.bounds[horizontal][0],
        receiver.bounds[horizontal][1],
        width,
        dtype=np.float64,
    )
    v_values = np.linspace(
        receiver.bounds[vertical][0],
        receiver.bounds[vertical][1],
        height,
        dtype=np.float64,
    )
    hh, vv = np.meshgrid(h_values, v_values, indexing="xy")
    points = np.zeros((height, width, 3), dtype=np.float64)
    index = {"x": 0, "y": 1, "z": 2}
    points[..., index[horizontal]] = hh
    points[..., index[vertical]] = vv
    points[..., index[receiver.plane_axis]] = receiver.plane_value
    return points, v_values


def _rgba_from_rgb(rgb: np.ndarray) -> np.ndarray:
    rgba = np.zeros((*rgb.shape[:2], 4), dtype=np.float32)
    rgba[..., 0:3] = np.maximum(rgb, 0.0)
    rgba[..., 3] = 1.0
    return rgba


def build_receiver_lightmaps(
    scene: Scene,
    projection: ProjectionSystem,
    workdir: str | Path,
    width: int = 384,
    height: int = 288,
) -> dict[str, Any]:
    workdir = Path(workdir)
    diagnostics = workdir / "diagnostics"
    diagnostics.mkdir(parents=True, exist_ok=True)
    document: dict[str, Any] = {
        "format": "GLV-LIGHTMAPS-V1",
        "scene_digest": scene.digest,
        "pattern_digest": projection.patterns.digest,
        "camera_dependent": False,
        "projection": "world-space inverse spot/aperture ray mapping",
        "receiver_spot_gain": RECEIVER_SPOT_GAIN,
        "maps": [],
    }
    for receiver_id in ("wall", "floor"):
        receiver = scene.receivers[receiver_id]
        points, _ = _receiver_grid(scene, receiver_id, width, height)
        flat_points = points.reshape(-1, 3)
        normals = np.broadcast_to(receiver.normal, flat_points.shape).copy()
        codes = np.full(flat_points.shape[0], 0 if receiver_id == "wall" else 1, dtype=np.int8)
        surface = receiver_surface_color(scene, flat_points, codes)
        contribution = projection.light_contributions(flat_points, normals)
        ambient = surface * scene.render.ambient_strength
        composite = ambient.copy()
        for light in sorted(scene.lights, key=lambda item: item.id.encode("utf-8")):
            composite += (
                surface
                * light.color[None, :]
                * contribution.by_light[light.id][:, None]
                * RECEIVER_SPOT_GAIN
            )
        composite = np.maximum(composite, 0.0).reshape(height, width, 3)
        composite_display = np.flipud(composite)
        composite_path = diagnostics / f"{receiver_id}_lightmap_linear.npy"
        composite_png = diagnostics / f"{receiver_id}_lightmap.png"
        atomic_write_npy(composite_path, composite_display.astype(np.float32))
        composite_png_data = encode_png_rgba(
            display_rgba8(scene, _rgba_from_rgb(composite_display))
        )
        atomic_write_bytes(composite_png, composite_png_data)
        light_files: dict[str, Any] = {}
        for light in sorted(scene.lights, key=lambda item: item.id.encode("utf-8")):
            single = (
                ambient
                + surface
                * light.color[None, :]
                * contribution.by_light[light.id][:, None]
                * RECEIVER_SPOT_GAIN
            )
            single = np.maximum(single, 0.0).reshape(height, width, 3)
            single_display = np.flipud(single)
            single_path = diagnostics / f"{receiver_id}_lightmap_{light.id}.npy"
            single_png = diagnostics / f"{receiver_id}_lightmap_{light.id}.png"
            atomic_write_npy(single_path, single_display.astype(np.float32))
            single_png_data = encode_png_rgba(
                display_rgba8(scene, _rgba_from_rgb(single_display))
            )
            atomic_write_bytes(single_png, single_png_data)
            light_files[light.id] = {
                "linear_npy": str(single_path.relative_to(workdir).as_posix()),
                "png": str(single_png.relative_to(workdir).as_posix()),
                "npy_sha256": sha256_bytes(
                    np.ascontiguousarray(single_display.astype(np.float32)).tobytes()
                ),
                "png_sha256": sha256_bytes(single_png_data),
            }
        document["maps"].append(
            {
                "receiver": receiver_id,
                "plane_axis": receiver.plane_axis,
                "plane_value": receiver.plane_value,
                "bounds": {
                    key: list(value) for key, value in receiver.bounds.items()
                },
                "resolution": [width, height],
                "orientation": (
                    "x right, y up (PNG row 0 is y maximum)"
                    if receiver_id == "wall"
                    else "x right, z up in world (PNG row 0 is z maximum)"
                ),
                "linear_npy": str(composite_path.relative_to(workdir).as_posix()),
                "png": str(composite_png.relative_to(workdir).as_posix()),
                "npy_sha256": sha256_bytes(
                    np.ascontiguousarray(composite_display.astype(np.float32)).tobytes()
                ),
                "png_sha256": sha256_bytes(composite_png_data),
                "per_light": light_files,
            }
        )
    document["digest"] = object_digest(document["maps"])
    return document
