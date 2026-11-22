"""Deterministic local sample scene generator and baseline manifests."""

from __future__ import annotations

import copy
import json
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np

from .binary import build_scene, load_scene
from .config import RenderConfig, parse_config
from .hashing import sha256_bytes
from .io_utils import atomic_write_bytes, json_bytes, jsonl_bytes, png_bytes_from_ppm
from .model import (
    MEDIUM_BOX,
    MEDIUM_SPHERE,
    Camera,
    DensityField,
    DirectionalLight,
    Medium,
    Scene,
)
from .renderer import render_scene


def default_sample_config(scene: Scene) -> RenderConfig:
    tile_size = 16 if scene.width <= 64 else 24
    raw = {
        "tile_size": tile_size,
        "max_parallelism": 4,
        "sample_budget": 2_000_000,
        "base_step": 0.10,
        "min_step": 0.008,
        "precision": "float",
        "degrade_sequence": [
            {"name": "S0_FULL", "step_multiplier": 1.0, "shadow_samples": 2},
            {"name": "S1_STEP_2X", "step_multiplier": 2.0, "shadow_samples": 2},
            {
                "name": "S2_STEP_2X_SHADOW_1",
                "step_multiplier": 2.0,
                "shadow_samples": 1,
            },
            {
                "name": "S3_STEP_4X_SHADOW_1",
                "step_multiplier": 4.0,
                "shadow_samples": 1,
            },
        ],
        "seam": {
            "enabled": True,
            "tolerance": 0.0,
            "truth_budget": 20_000_000,
            "truth_stage": "S0_FULL",
        },
        "limits": {"max_parallelism": 8, "max_sample_budget": 50_000_000},
    }
    return parse_config(json.dumps(raw))


def _split_chunks(
    full: np.ndarray, chunk_shape: tuple[int, int, int]
) -> dict[tuple[int, int, int], np.ndarray]:
    nz, ny, nx = full.shape
    cz, cy, cx = chunk_shape[2], chunk_shape[1], chunk_shape[0]
    chunks: dict[tuple[int, int, int], np.ndarray] = {}
    for kz in range((nz + cz - 1) // cz):
        for ky in range((ny + cy - 1) // cy):
            for kx in range((nx + cx - 1) // cx):
                chunks[(kx, ky, kz)] = np.ascontiguousarray(
                    full[
                        kz * cz : min((kz + 1) * cz, nz),
                        ky * cy : min((ky + 1) * cy, ny),
                        kx * cx : min((kx + 1) * cx, nx),
                    ]
                )
    return chunks


def _gradient_field(
    field_id: int,
    dims: tuple[int, int, int],
    chunk_shape: tuple[int, int, int],
    rng: np.random.Generator,
) -> DensityField:
    nx, ny, nz = dims
    x = np.linspace(0.0, 1.0, nx)[None, None, :]
    y = np.linspace(0.0, 1.0, ny)[None, :, None]
    z = np.linspace(0.0, 1.0, nz)[:, None, None]
    full = 0.35 + 1.55 * np.exp(-((x - 0.58) ** 2 + (y - 0.48) ** 2 + (z - 0.52) ** 2) / 0.18)
    full *= 0.90 + 0.10 * np.sin((x * 5.0 + y * 3.0 + z * 2.0) * np.pi)
    full += rng.normal(0.0, 0.015, size=full.shape)
    return DensityField(
        id=field_id,
        dims=dims,
        chunk_shape=chunk_shape,
        chunks=_split_chunks(np.clip(full, 0.0, None), chunk_shape),
    )


def _blob_field(
    field_id: int,
    dims: tuple[int, int, int],
    chunk_shape: tuple[int, int, int],
    rng: np.random.Generator,
) -> DensityField:
    nx, ny, nz = dims
    x = np.linspace(0.0, 1.0, nx)[None, None, :]
    y = np.linspace(0.0, 1.0, ny)[None, :, None]
    z = np.linspace(0.0, 1.0, nz)[:, None, None]
    full = (
        0.10
        + 1.8 * np.exp(-((x - 0.32) ** 2 + (y - 0.55) ** 2 + (z - 0.5) ** 2) / 0.035)
        + 1.25 * np.exp(-((x - 0.74) ** 2 + (y - 0.38) ** 2 + (z - 0.45) ** 2) / 0.055)
    )
    full += rng.normal(0.0, 0.01, size=full.shape)
    return DensityField(
        id=field_id,
        dims=dims,
        chunk_shape=chunk_shape,
        chunks=_split_chunks(np.clip(full, 0.0, None), chunk_shape),
    )


def _layered_field(
    field_id: int,
    dims: tuple[int, int, int],
    chunk_shape: tuple[int, int, int],
    rng: np.random.Generator,
) -> DensityField:
    nx, ny, nz = dims
    x = np.linspace(0.0, 1.0, nx)[None, None, :]
    y = np.linspace(0.0, 1.0, ny)[None, :, None]
    z = np.linspace(0.0, 1.0, nz)[:, None, None]
    full = 0.55 + 0.5 * np.sin(z * np.pi * 5.0) ** 2 + 0.25 * np.cos(x * np.pi * 3.0)
    full = full * (0.75 + 0.35 * y)
    full += rng.normal(0.0, 0.02, size=full.shape)
    return DensityField(
        id=field_id,
        dims=dims,
        chunk_shape=chunk_shape,
        chunks=_split_chunks(np.clip(full, 0.02, None), chunk_shape),
    )


def _uniform_field(
    field_id: int,
    dims: tuple[int, int, int],
    chunk_shape: tuple[int, int, int],
    value: float,
) -> DensityField:
    full = np.full((dims[2], dims[1], dims[0]), value, dtype=np.float64)
    return DensityField(
        id=field_id,
        dims=dims,
        chunk_shape=chunk_shape,
        chunks=_split_chunks(full, chunk_shape),
    )


def _camera(width: int, height: int, eye: tuple[float, float, float]) -> Camera:
    focal = 1.28 * width
    intrinsics = np.array(
        [
            [focal, 0.0, (width - 1) * 0.5],
            [0.0, focal, (height - 1) * 0.5],
            [0.0, 0.0, 1.0],
        ],
        dtype=np.float64,
    )
    world_to_camera = np.eye(4, dtype=np.float64)
    world_to_camera[:3, 3] = -np.asarray(eye, dtype=np.float64)
    return Camera(intrinsics, world_to_camera)


def _box_medium(
    medium_id: int,
    field_id: int,
    lo: tuple[float, float, float],
    hi: tuple[float, float, float],
    sigma_s: tuple[float, float, float],
    sigma_a: tuple[float, float, float],
    g: float,
    density_scale: float = 1.0,
) -> Medium:
    return Medium(
        id=medium_id,
        kind=MEDIUM_BOX,
        density_field_id=field_id,
        sigma_s=np.array(sigma_s, dtype=np.float64),
        sigma_a=np.array(sigma_a, dtype=np.float64),
        g=g,
        density_scale=density_scale,
        shape_a=np.array(lo, dtype=np.float64),
        shape_b=np.array([hi[0], hi[1], hi[2], 0.0], dtype=np.float64),
    )


def _sphere_medium(
    medium_id: int,
    field_id: int,
    center: tuple[float, float, float],
    radius: float,
    sigma_s: tuple[float, float, float],
    sigma_a: tuple[float, float, float],
    g: float,
    density_scale: float = 1.0,
) -> Medium:
    return Medium(
        id=medium_id,
        kind=MEDIUM_SPHERE,
        density_field_id=field_id,
        sigma_s=np.array(sigma_s, dtype=np.float64),
        sigma_a=np.array(sigma_a, dtype=np.float64),
        g=g,
        density_scale=density_scale,
        shape_a=np.array(center, dtype=np.float64),
        shape_b=np.array([radius, 0.0, 0.0, 0.0], dtype=np.float64),
    )


def make_sample_scenes(seed: int = 20261122) -> list[dict[str, Any]]:
    specs: list[dict[str, Any]] = []

    rng = np.random.default_rng(seed + 1)
    field = _gradient_field(100, (10, 10, 10), (4, 4, 4), rng)
    scene = Scene(
        width=48,
        height=32,
        camera=_camera(48, 32, (0.0, 0.05, -3.4)),
        light=DirectionalLight([0.18, 0.28, 1.0], [1.20, 1.02, 0.82]),
        media=[
            _box_medium(
                10,
                100,
                (-0.95, -0.72, -0.55),
                (0.95, 0.72, 0.55),
                (0.22, 0.17, 0.12),
                (0.035, 0.030, 0.025),
                0.18,
            )
        ],
        density_fields={100: field},
    )
    specs.append(
        {
            "name": "gradient_smoke_48x32",
            "description": "single gradient smoke box",
            "scene": scene,
            "density_distributions": ["gaussian_gradient"],
        }
    )

    rng = np.random.default_rng(seed + 2)
    smoke_field = _blob_field(200, (12, 12, 12), (4, 4, 4), rng)
    glass_field = _uniform_field(201, (4, 4, 4), (2, 2, 2), 1.0)
    scene = Scene(
        width=64,
        height=44,
        camera=_camera(64, 44, (0.0, 0.10, -3.5)),
        light=DirectionalLight([-0.22, 0.32, 1.0], [1.10, 1.05, 0.95]),
        media=[
            _sphere_medium(
                20,
                201,
                (0.0, 0.0, 0.0),
                1.15,
                (0.035, 0.030, 0.025),
                (0.018, 0.016, 0.014),
                0.10,
                density_scale=0.12,
            ),
            _box_medium(
                21,
                200,
                (-0.62, -0.50, -0.45),
                (0.62, 0.50, 0.45),
                (0.24, 0.19, 0.13),
                (0.045, 0.038, 0.030),
                0.22,
            ),
        ],
        density_fields={200: smoke_field, 201: glass_field},
    )
    specs.append(
        {
            "name": "smoke_in_glass_64x44",
            "description": "two nested media with a glass sphere",
            "scene": scene,
            "density_distributions": ["dual_blob", "uniform_glass"],
        }
    )

    rng = np.random.default_rng(seed + 3)
    left_field = _layered_field(300, (12, 10, 8), (4, 4, 4), rng)
    right_field = _blob_field(301, (10, 12, 10), (4, 4, 4), rng)
    shell_field = _uniform_field(302, (4, 4, 4), (2, 2, 2), 1.0)
    scene = Scene(
        width=72,
        height=48,
        camera=_camera(72, 48, (0.0, 0.16, -3.75)),
        light=DirectionalLight([0.35, 0.18, 1.0], [1.15, 1.00, 0.88]),
        media=[
            _sphere_medium(
                30,
                302,
                (0.0, 0.0, 0.0),
                1.32,
                (0.030, 0.028, 0.022),
                (0.020, 0.018, 0.015),
                0.08,
                density_scale=0.10,
            ),
            _box_medium(
                31,
                300,
                (-0.78, -0.52, -0.42),
                (-0.09, 0.56, 0.48),
                (0.20, 0.16, 0.11),
                (0.040, 0.034, 0.028),
                0.24,
            ),
            _box_medium(
                32,
                301,
                (0.09, -0.55, -0.44),
                (0.82, 0.50, 0.42),
                (0.13, 0.20, 0.17),
                (0.032, 0.040, 0.036),
                0.12,
            ),
        ],
        density_fields={300: left_field, 301: right_field, 302: shell_field},
    )
    specs.append(
        {
            "name": "dual_plume_glass_72x48",
            "description": "three nested media with layered and blob fields",
            "scene": scene,
            "density_distributions": ["layered", "dual_blob", "uniform_glass"],
        }
    )
    return specs


def generate_samples(
    output_dir: str | Path,
    seed: int = 20261122,
    render_baselines: bool = True,
) -> dict[str, Any]:
    directory = Path(output_dir)
    directory.mkdir(parents=True, exist_ok=True)
    specs = make_sample_scenes(seed)
    config = default_sample_config(specs[0]["scene"])
    atomic_write_bytes(directory / "render_config.json", json_bytes(config.to_dict()))
    manifest: dict[str, Any] = {
        "schema": "volrender.samples.v1",
        "seed": seed,
        "generator": "volrender.samples.generate_samples",
        "config": config.to_dict(),
        "samples": [],
    }
    for spec in specs:
        scene: Scene = spec["scene"]
        name = spec["name"]
        scene_bytes = build_scene(scene)
        scene_path = directory / f"{name}.vscn"
        atomic_write_bytes(scene_path, scene_bytes)
        entry: dict[str, Any] = {
            "name": name,
            "description": spec["description"],
            "scene_file": scene_path.name,
            "resolution": [scene.width, scene.height],
            "media_count": len(scene.media),
            "density_distributions": spec["density_distributions"],
            "scene_hash": sha256_bytes(scene_bytes),
        }
        if render_baselines:
            loaded = load_scene(scene_path)
            scene_config = default_sample_config(loaded)
            result = render_scene(loaded, scene_config, validate_seams=True)
            atomic_write_bytes(directory / f"{name}.baseline.ppm", result.image_ppm)
            atomic_write_bytes(
                directory / f"{name}.baseline.png",
                png_bytes_from_ppm(result.image_ppm),
            )
            atomic_write_bytes(
                directory / f"{name}.baseline_trace.jsonl",
                jsonl_bytes(result.trace),
            )
            entry["baseline"] = {
                "pixel_hash": result.pixel_hash,
                "image_hash": result.image_hash,
                "summary_hash": result.summary["summary_hash"],
                "content_hash": result.summary["render_hash"],
                "seam_check": result.summary["seam_check"],
                "budget": result.summary["budget"],
                "degradation": result.summary["degradation"]["counts_by_stage"],
            }
        manifest["samples"].append(entry)
    manifest["manifest_hash"] = sha256_bytes(json_bytes(manifest))
    atomic_write_bytes(directory / "manifest.json", json_bytes(manifest))
    return manifest


def verify_samples(
    sample_dir: str | Path,
    seed: int = 20261122,
) -> dict[str, Any]:
    directory = Path(sample_dir)
    manifest = json.loads((directory / "manifest.json").read_text(encoding="utf-8"))
    failures: list[dict[str, Any]] = []
    checked = 0
    for entry in manifest.get("samples", []):
        scene_path = directory / entry["scene_file"]
        scene_bytes = scene_path.read_bytes()
        scene = load_scene(scene_bytes)
        config = default_sample_config(scene)
        result = render_scene(scene, config, validate_seams=True)
        if sha256_bytes(scene_bytes) != entry.get("scene_hash"):
            failures.append(
                {
                    "sample": entry["name"],
                    "field": "scene_hash",
                    "expected": entry.get("scene_hash"),
                    "actual": sha256_bytes(scene_bytes),
                }
            )
        baseline = entry.get("baseline")
        if baseline is None:
            continue
        checked += 1
        expected = {
            "pixel_hash": result.pixel_hash,
            "image_hash": result.image_hash,
            "summary_hash": result.summary["summary_hash"],
        }
        for key, value in expected.items():
            if baseline.get(key) != value:
                failures.append(
                    {
                        "sample": entry["name"],
                        "field": key,
                        "expected": baseline.get(key),
                        "actual": value,
                    }
                )
    return {
        "ok": not failures,
        "checked": checked,
        "failures": failures,
    }


__all__ = [
    "default_sample_config",
    "generate_samples",
    "make_sample_scenes",
    "verify_samples",
]
