"""Deterministic local scene and target-image generator."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .params import GROUPS, canonical_json_bytes
from .renderer import render_image
from .scene import load_scene


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(65536), b""):
            digest.update(block)
    return digest.hexdigest()


def _write_json(path: Path, payload: object) -> None:
    encoded = json.dumps(
        payload,
        sort_keys=True,
        indent=2,
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    temporary = path.with_name(f".{path.name}.tmp")
    temporary.write_bytes(encoded)
    temporary.replace(path)


def generate_sample(
    output_dir: str | Path,
    *,
    seed: int = 7,
    width: int = 24,
    height: int = 18,
    sphere_count: int = 2,
    softness: float = 0.075,
    noise: float = 0.002,
    name: str = "sample",
) -> dict[str, Any]:
    if width <= 0 or height <= 0:
        raise ValueError("image dimensions must be positive")
    if sphere_count < 1:
        raise ValueError("sphere_count must be at least one")
    directory = Path(output_dir).resolve()
    directory.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(seed)
    fx = float(max(width, height)) * 3.0
    fy = fx * (1.0 + 0.04 * float(rng.uniform(-1.0, 1.0)))
    camera_yaw = float(rng.uniform(-0.09, 0.09))
    camera_pitch = float(rng.uniform(-0.07, 0.07))
    camera_roll = float(rng.uniform(-0.04, 0.04))
    light_raw = np.asarray(
        [
            float(rng.uniform(-0.35, 0.35)),
            float(rng.uniform(0.3, 0.8)),
            float(rng.uniform(0.55, 1.0)),
        ],
        dtype=np.float64,
    )
    light_raw /= np.linalg.norm(light_raw)
    spheres: list[dict[str, Any]] = []
    albedos: list[list[float]] = []
    for index in range(sphere_count):
        angle = 2.0 * math.pi * (index + 0.35 * rng.random()) / sphere_count
        radius = float(rng.uniform(0.42, 0.86))
        center = [
            float(0.95 * math.cos(angle) * rng.uniform(0.35, 1.0)),
            float(0.55 * math.sin(angle) * rng.uniform(0.35, 1.0)),
            float(rng.uniform(-0.9, 0.2)),
        ]
        spheres.append({"center": center, "radius": radius})
        albedos.append(
            [
                float(rng.uniform(0.18, 0.92)),
                float(rng.uniform(0.18, 0.92)),
                float(rng.uniform(0.18, 0.92)),
            ]
        )
    plane_normal = np.asarray(
        [
            float(rng.uniform(-0.08, 0.08)),
            float(rng.uniform(-0.08, 0.08)),
            1.0,
        ],
        dtype=np.float64,
    )
    plane_normal /= np.linalg.norm(plane_normal)
    camera_position = [
        float(rng.uniform(-0.2, 0.2)),
        float(rng.uniform(-0.12, 0.12)),
        float(rng.uniform(4.7, 5.5)),
    ]
    scene_payload: dict[str, Any] = {
        "format": "inverse-imaging-scene",
        "format_version": 1,
        "seed": seed,
        "resolution": {"width": width, "height": height},
        "camera": {
            "position": camera_position,
            "rotation": [camera_yaw, camera_pitch, camera_roll],
            "intrinsics": {
                "fx": fx,
                "fy": fy,
                "cx": width / 2.0,
                "cy": height / 2.0,
            },
        },
        "geometry": {
            "spheres": spheres,
            "plane": {
                "point": [0.0, 0.0, -2.4],
                "normal": plane_normal.tolist(),
            },
        },
        "material": {
            "sphere_albedo": albedos,
            "plane_albedo": [
                float(rng.uniform(0.12, 0.42)),
                float(rng.uniform(0.12, 0.42)),
                float(rng.uniform(0.12, 0.42)),
            ],
            "ambient": float(rng.uniform(0.025, 0.075)),
        },
        "light": {
            "direction": light_raw.tolist(),
            "intensity": float(rng.uniform(0.75, 1.45)),
        },
        "settings": {
            "softness": float(softness),
            "shadow_samples": 9 if softness > 0.1 else 5,
            "plane_size": 30.0,
        },
        "target": "target.npy",
    }
    scene_path = directory / "scene.json"
    target_path = directory / "target.npy"
    np.save(target_path, np.zeros((height, width, 3), dtype=np.float64))
    _write_json(scene_path, scene_payload)
    preview_scene = load_scene(scene_path)
    target = render_image(preview_scene.params, preview_scene, chunk_size=min(width, height))
    if noise > 0.0:
        target = target + rng.normal(0.0, noise, size=target.shape)
    target = np.ascontiguousarray(np.clip(target, 0.0, 1.0), dtype=np.float64)
    np.save(target_path, target)
    scene = load_scene(scene_path)
    partition = {
        "groups": {
            "camera": [key for key in scene.params.keys("camera")],
            "geometry": [key for key in scene.params.keys("geometry")],
            "material": [key for key in scene.params.keys("material")],
            "light": [key for key in scene.params.keys("light")],
        },
        "schedule": list(GROUPS),
    }
    partition_path = directory / "groups.json"
    _write_json(partition_path, partition)
    result = {
        "name": name,
        "seed": seed,
        "directory": str(directory),
        "scene": str(scene_path),
        "target": str(target_path),
        "partition": str(partition_path),
        "scene_id": scene.scene_id,
        "resolution": {"width": width, "height": height},
        "sphere_count": sphere_count,
        "softness": float(softness),
        "scene_sha256": _sha256_file(scene_path),
        "target_sha256": _sha256_file(target_path),
        "partition_sha256": _sha256_file(partition_path),
        "target_mean": float(np.mean(target)),
        "target_std": float(np.std(target)),
    }
    return result


def generate_matrix(
    root: str | Path,
    *,
    seed: int = 7,
    include_large: bool = False,
) -> dict[str, Any]:
    root_path = Path(root).resolve()
    specs = [
        {
            "name": "low_16x12",
            "seed": seed,
            "width": 16,
            "height": 12,
            "sphere_count": 1,
            "softness": 0.035,
            "noise": 0.001,
        },
        {
            "name": "medium_24x18",
            "seed": seed + 1,
            "width": 24,
            "height": 18,
            "sphere_count": 2,
            "softness": 0.075,
            "noise": 0.002,
        },
        {
            "name": "high_32x24",
            "seed": seed + 2,
            "width": 32,
            "height": 24,
            "sphere_count": 3,
            "softness": 0.12,
            "noise": 0.003,
        },
    ]
    if include_large:
        specs.append(
            {
                "name": "large_48x36",
                "seed": seed + 3,
                "width": 48,
                "height": 36,
                "sphere_count": 3,
                "softness": 0.09,
                "noise": 0.002,
            }
        )
    samples = [
        generate_sample(root_path / spec["name"], **spec) for spec in specs
    ]
    manifest = {
        "format": "inverse-imaging-sample-matrix",
        "format_version": 1,
        "seed": seed,
        "samples": samples,
        "manifest_hash": hashlib.sha256(
            canonical_json_bytes(
                [
                    {
                        key: value
                        for key, value in sample.items()
                        if key not in ("directory", "scene", "target", "partition")
                    }
                    for sample in samples
                ]
            )
        ).hexdigest(),
    }
    _write_json(root_path / "manifest.json", manifest)
    return manifest

