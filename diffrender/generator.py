"""Deterministic local dataset generator for inverse-rendering exercises."""

from __future__ import annotations

import hashlib
import json
import math
import os
import random
import tempfile
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np
import torch

from .errors import fail
from .params import (
    build_layout,
    physical_values_from_raw,
    raw_from_scene,
    scene_from_raw,
)
from .renderer import RenderConfig, render_image
from .schema import (
    CameraSpec,
    PlaneSpec,
    RectLightSpec,
    SceneSpec,
    SphereSpec,
    TargetImageSpec,
)


GENERATOR_VERSION = "1.0.0"
FORMAT_VERSION = 1


def _atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except Exception:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def _atomic_write_json(path: Path, value: Any) -> None:
    payload = json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    _atomic_write_bytes(path, payload.encode("utf-8"))


def _look_at(camera_position: list[float], target: list[float]) -> list[list[float]]:
    position = torch.tensor(camera_position, dtype=torch.float64)
    look = torch.tensor(target, dtype=torch.float64)
    forward = torch.nn.functional.normalize(look - position, dim=0)
    world_up = torch.tensor((0.0, 1.0, 0.0), dtype=torch.float64)
    right = torch.nn.functional.normalize(torch.cross(forward, world_up, dim=0), dim=0)
    down = torch.cross(forward, right, dim=0)
    rotation = torch.stack((right, down, forward))
    translation = -(rotation @ position)
    matrix = torch.eye(4, dtype=torch.float64)
    matrix[:3, :3] = rotation
    matrix[:3, 3] = translation
    return matrix.tolist()


def _camera(
    camera_id: str,
    position: list[float],
    target: list[float],
    width: int,
    height: int,
    *,
    focal_scale: float = 0.95,
) -> CameraSpec:
    fx = focal_scale * width
    fy = focal_scale * width
    cx = width * 0.5
    cy = height * 0.5
    return CameraSpec(
        id=camera_id,
        intrinsics=((fx, 0.0, cx), (0.0, fy, cy), (0.0, 0.0, 1.0)),
        world_to_camera=tuple(tuple(row) for row in _look_at(position, target)),
    )


def _single_truth(seed: int) -> tuple[SceneSpec, tuple[TargetImageSpec, ...]]:
    width, height = 64, 48
    cameras = (
        _camera("cam_front", [0.0, 1.15, -3.9], [0.0, 0.72, 0.0], width, height),
        _camera(
            "cam_left",
            [-2.45, 1.30, -3.10],
            [0.0, 0.66, 0.12],
            width,
            height,
            focal_scale=0.92,
        ),
    )
    scene = SceneSpec(
        scene_id="single",
        background=(0.025, 0.030, 0.045),
        ambient=0.045,
        soft_shadow_samples=4,
        ray_epsilon=1.0e-3,
        target_seed=seed + 1000,
        geometries=(
            PlaneSpec(
                id="ground",
                point=(0.0, 0.0, 0.0),
                normal=(0.0, 1.0, 0.0),
                albedo=(0.40, 0.43, 0.48),
            ),
            SphereSpec(
                id="sphere_0",
                center=(0.0, 0.86, 0.0),
                radius=0.86,
                albedo=(0.78, 0.35, 0.20),
            ),
        ),
        lights=(
            RectLightSpec(
                id="key_light",
                center=(1.5, 3.2, -1.6),
                normal=(0.0, -1.0, 0.0),
                half_size=(1.15, 0.75),
                intensity=25.0,
            ),
        ),
        cameras=cameras,
    )
    targets = tuple(
        TargetImageSpec(
            id=f"single_{camera.id}",
            camera_id=camera.id,
            path=f"single/images/{camera.id}.npy",
            width=width,
            height=height,
        )
        for camera in cameras
    )
    return scene, targets


def _multi_truth(seed: int) -> tuple[SceneSpec, tuple[TargetImageSpec, ...]]:
    width, height = 72, 56
    cameras = (
        _camera("cam_front", [0.0, 1.45, -4.8], [0.0, 0.72, 0.05], width, height),
        _camera(
            "cam_left",
            [-3.85, 1.55, -2.85],
            [0.0, 0.72, 0.10],
            width,
            height,
            focal_scale=0.90,
        ),
        _camera(
            "cam_right",
            [3.65, 1.30, -2.45],
            [0.08, 0.65, 0.15],
            width,
            height,
            focal_scale=0.90,
        ),
    )
    scene = SceneSpec(
        scene_id="multi",
        background=(0.022, 0.028, 0.038),
        ambient=0.045,
        soft_shadow_samples=5,
        ray_epsilon=1.0e-3,
        target_seed=seed + 2000,
        geometries=(
            PlaneSpec(
                id="ground",
                point=(0.0, 0.0, 0.0),
                normal=(0.0, 1.0, 0.0),
                albedo=(0.37, 0.40, 0.45),
            ),
            SphereSpec(
                id="sphere_red",
                center=(-1.12, 0.66, 0.0),
                radius=0.66,
                albedo=(0.82, 0.28, 0.17),
            ),
            SphereSpec(
                id="sphere_green",
                center=(0.05, 0.78, 0.36),
                radius=0.78,
                albedo=(0.24, 0.68, 0.34),
            ),
            SphereSpec(
                id="sphere_blue",
                center=(1.28, 0.52, -0.12),
                radius=0.52,
                albedo=(0.20, 0.37, 0.82),
            ),
        ),
        lights=(
            RectLightSpec(
                id="key_light",
                center=(0.4, 3.5, -2.0),
                normal=(0.0, -1.0, 0.0),
                half_size=(1.8, 1.0),
                intensity=30.0,
            ),
            RectLightSpec(
                id="fill_light",
                center=(-2.7, 2.5, 1.7),
                normal=(0.0, -1.0, 0.0),
                half_size=(1.0, 0.65),
                intensity=8.0,
            ),
        ),
        cameras=cameras,
    )
    targets = tuple(
        TargetImageSpec(
            id=f"multi_{camera.id}",
            camera_id=camera.id,
            path=f"multi/images/{camera.id}.npy",
            width=width,
            height=height,
        )
        for camera in cameras
    )
    return scene, targets


def _perturb_scene(
    truth_scene: SceneSpec,
    seed: int,
) -> SceneSpec:
    rng = random.Random(seed + 77)
    geometries: list[SphereSpec | PlaneSpec] = []
    for geometry in truth_scene.geometries:
        if isinstance(geometry, SphereSpec):
            center = tuple(value + rng.uniform(-0.10, 0.10) for value in geometry.center)
            radius = geometry.radius * rng.uniform(0.84, 1.13)
            albedo = tuple(
                min(0.96, max(0.03, value + rng.uniform(-0.09, 0.09)))
                for value in geometry.albedo
            )
            geometries.append(
                SphereSpec(
                    id=geometry.id,
                    center=center,  # type: ignore[arg-type]
                    radius=radius,
                    albedo=albedo,  # type: ignore[arg-type]
                )
            )
        else:
            albedo = tuple(
                min(0.96, max(0.03, value + rng.uniform(-0.06, 0.06)))
                for value in geometry.albedo
            )
            geometries.append(replace(geometry, albedo=albedo))  # type: ignore[arg-type]
    lights = tuple(
        replace(
            light,
            intensity=max(0.5, light.intensity * rng.uniform(0.82, 1.18)),
        )
        for light in truth_scene.lights
    )
    return replace(truth_scene, geometries=tuple(geometries), lights=lights)


def _truth_payload(scene: SceneSpec) -> dict[str, Any]:
    geometries: dict[str, Any] = {}
    for geometry in scene.geometries:
        if isinstance(geometry, SphereSpec):
            geometries[geometry.id] = {
                "center": list(geometry.center),
                "radius": geometry.radius,
                "albedo": list(geometry.albedo),
            }
        else:
            geometries[geometry.id] = {
                "center": list(geometry.point),
                "albedo": list(geometry.albedo),
            }
    return {
        "geometries": geometries,
        "lights": {
            light.id: {"intensity": light.intensity} for light in scene.lights
        },
    }


def _scene_payload(scene: SceneSpec) -> dict[str, Any]:
    geometries: list[dict[str, Any]] = []
    for geometry in scene.geometries:
        if isinstance(geometry, SphereSpec):
            geometries.append(
                {
                    "id": geometry.id,
                    "type": "sphere",
                    "center": list(geometry.center),
                    "radius": geometry.radius,
                    "albedo": list(geometry.albedo),
                }
            )
        else:
            geometries.append(
                {
                    "id": geometry.id,
                    "type": "plane",
                    "point": list(geometry.point),
                    "normal": list(geometry.normal),
                    "albedo": list(geometry.albedo),
                }
            )
    return {
        "scene_id": scene.scene_id,
        "background": list(scene.background),
        "ambient": scene.ambient,
        "render": {
            "soft_shadow_samples": scene.soft_shadow_samples,
            "ray_epsilon": scene.ray_epsilon,
            "target_seed": scene.target_seed,
        },
        "geometries": geometries,
        "lights": [
            {
                "id": light.id,
                "type": "rect",
                "center": list(light.center),
                "normal": list(light.normal),
                "half_size": list(light.half_size),
                "intensity": light.intensity,
            }
            for light in scene.lights
        ],
        "cameras": [
            {
                "id": camera.id,
                "intrinsics": [list(row) for row in camera.intrinsics],
                "world_to_camera": [list(row) for row in camera.world_to_camera],
            }
            for camera in scene.cameras
        ],
    }


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def generate_dataset(
    output_dir: str | os.PathLike[str],
    *,
    seed: int = 20261123,
    force: bool = False,
) -> dict[str, Any]:
    root = Path(output_dir).resolve()
    manifest_path = root / "manifest.json"
    if manifest_path.exists() and not force:
        fail(
            "E_STATE",
            "dataset already exists; pass --force to regenerate",
            "manifest.json",
            path=str(manifest_path),
        )
    root.mkdir(parents=True, exist_ok=True)
    torch.set_num_threads(1)
    groups: list[dict[str, Any]] = []
    generated: list[dict[str, Any]] = []

    for group_id, builder in (("single", _single_truth), ("multi", _multi_truth)):
        truth_scene, targets = builder(seed)
        initial_scene = _perturb_scene(truth_scene, seed)
        layout = build_layout(truth_scene)
        truth_raw = raw_from_scene(truth_scene, layout)
        truth_values = physical_values_from_raw(truth_scene, layout, truth_raw)
        scene_path = root / group_id / "scene.json"
        target_path = root / group_id / "targets.json"
        _atomic_write_json(scene_path, _scene_payload(initial_scene))
        target_payload: dict[str, Any] = {
            "format_version": FORMAT_VERSION,
            "targets": [
                {
                    "id": target.id,
                    "camera_id": target.camera_id,
                    "path": target.path,
                    "width": target.width,
                    "height": target.height,
                }
                for target in targets
            ],
            "truth": _truth_payload(truth_scene),
        }
        _atomic_write_json(target_path, target_payload)
        camera_by_id = {camera.id: camera for camera in truth_scene.cameras}
        truth_render_scene = scene_from_raw(truth_scene, layout, truth_raw)
        image_entries: list[dict[str, Any]] = []
        for image_index, target in enumerate(targets):
            render_config = RenderConfig(
                shadow_samples=truth_scene.soft_shadow_samples,
                ray_epsilon=truth_scene.ray_epsilon,
                downsample_factor=1,
                base_seed=truth_scene.target_seed,
            )
            with torch.no_grad():
                image = render_image(
                    truth_render_scene,
                    camera_by_id[target.camera_id],
                    target,
                    render_config,
                    seed_offset=image_index,
                )
            image_path = root / target.path
            image_path.parent.mkdir(parents=True, exist_ok=True)
            buffer = _npy_bytes(image.detach().cpu().numpy().astype(np.float64))
            _atomic_write_bytes(image_path, buffer)
            image_entries.append(
                {
                    "id": target.id,
                    "path": target.path,
                    "width": target.width,
                    "height": target.height,
                    "sha256": _sha256(image_path),
                    "dynamic_range": [
                        float(image.min().item()),
                        float(image.max().item()),
                    ],
                }
            )
        groups.append(
            {
                "group_id": group_id,
                "scene_path": f"{group_id}/scene.json",
                "target_manifest_path": f"{group_id}/targets.json",
                "image_count": len(targets),
                "object_count": len(truth_scene.geometries),
                "resolution": [targets[0].width, targets[0].height],
            }
        )
        generated.append(
            {
                "group_id": group_id,
                "object_count": len(truth_scene.geometries),
                "light_count": len(truth_scene.lights),
                "image_count": len(targets),
                "images": image_entries,
                "truth_parameter_count": len(truth_values),
                "scene_sha256": _sha256(scene_path),
                "targets_sha256": _sha256(target_path),
            }
        )

    manifest = {
        "format_version": FORMAT_VERSION,
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "groups": groups,
    }
    _atomic_write_json(manifest_path, manifest)
    return {
        "ok": True,
        "dataset": str(root),
        "seed": seed,
        "manifest": str(manifest_path),
        "groups": generated,
    }


def _npy_bytes(array: np.ndarray) -> bytes:
    import io

    buffer = io.BytesIO()
    np.save(buffer, array, allow_pickle=False)
    return buffer.getvalue()

