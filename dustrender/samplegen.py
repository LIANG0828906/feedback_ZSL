"""Deterministic local scene sample generator."""

from __future__ import annotations

import json
import struct
from pathlib import Path
from typing import Any

import numpy as np

from .config import RenderConfig
from .scene import MAGIC, Scene
from .scheduler import RenderScheduler
from .util import atomic_write_bytes, atomic_write_json, sha256_bytes

GENERATOR_VERSION = 1
BASE_SEED = 20261121


def _camera(width: int, height: int) -> dict[str, Any]:
    return {
        "far": 20.0,
        "intrinsics": [
            [width * 1.18, 0.0, width * 0.5],
            [0.0, height * 1.48, height * 0.5],
            [0.0, 0.0, 1.0],
        ],
        "near": 0.05,
        "world_to_camera": [
            [0.9997098084674824, 0.0, -0.024089392975120055, -3.9317374333620395e-18],
            [-0.0011592530612184945, 0.9988414188723856, -0.048109002040567526, 0.019976828377447693],
            [0.024061483459043395, 0.04812296691808678, 0.998551563550301, -4.155057271124909],
            [0.0, 0.0, 0.0, 1.0],
        ],
    }


def _density_values(distribution: str, seed: int) -> np.ndarray:
    grid = (12, 10, 12)
    x, y, z = np.meshgrid(
        np.arange(grid[0], dtype=np.float64),
        np.arange(grid[1], dtype=np.float64),
        np.arange(grid[2], dtype=np.float64),
        indexing="ij",
    )
    phase = float(seed % 97) / 17.0
    if distribution == "gradient":
        value = (
            0.08
            + 0.88
            * np.exp(
                -0.34 * x
                - 0.21 * y
                - 0.28 * z
                + 0.035 * np.sin(0.7 * x + 0.4 * y - 0.3 * z + phase)
            )
        )
    elif distribution == "wispy":
        wave = (
            np.sin(0.63 * x + 0.37 * y + 0.19 * z + phase)
            + 0.55 * np.cos(0.29 * x - 0.71 * y + 0.43 * z)
            + 0.25 * np.sin(1.21 * x + 0.83 * z - 0.31 * y)
        )
        envelope = np.exp(-((x - 5.5) ** 2 + (y - 4.0) ** 2 + (z - 6.0) ** 2) / 55.0)
        value = 0.04 + 0.92 * envelope * np.clip(0.55 + 0.28 * wave, 0.0, 1.0)
    elif distribution == "uniform":
        value = 0.55 + 0.08 * np.sin(0.41 * x + 0.37 * y + 0.23 * z + phase) * np.cos(
            0.27 * x - 0.31 * y + 0.19 * z
        )
    else:
        raise ValueError(f"unknown density distribution {distribution!r}")
    return np.clip(value, 0.0, 1.0).astype("<f4")


def _density_field(distribution: str, seed: int) -> tuple[dict[str, Any], bytes]:
    values = _density_values(distribution, seed)
    blocks: list[dict[str, Any]] = []
    payload_parts: list[bytes] = []
    offset = 0
    block_shape = (4, 5, 6)
    for bx in range(3):
        for by in range(2):
            for bz in range(2):
                origin = (bx * 4, by * 5, bz * 6)
                chunk = values[
                    origin[0] : origin[0] + 4,
                    origin[1] : origin[1] + 5,
                    origin[2] : origin[2] + 6,
                ]
                raw = np.ascontiguousarray(chunk, dtype="<f4").tobytes(order="C")
                payload_parts.append(raw)
                blocks.append(
                    {
                        "index": [bx, by, bz],
                        "origin": list(origin),
                        "shape": list(block_shape),
                        "offset": offset,
                        "count": int(block_shape[0] * block_shape[1] * block_shape[2]),
                        "hash": sha256_bytes(raw),
                    }
                )
                offset += len(raw)
    field = {
        "name": "main",
        "grid": [12, 10, 12],
        "bounds_min": [-2.3, -2.1, -2.3],
        "bounds_max": [2.3, 2.0, 2.3],
        "default": 0.0,
        "version": 1,
        "blocks": blocks,
    }
    return field, b"".join(payload_parts)


def _media(count: int) -> list[dict[str, Any]]:
    smoke = {
        "id": "smoke_core",
        "shape": {"type": "sphere", "center": [-0.42, 0.02, 0.04], "radius": 0.48},
        "scattering": [1.15, 0.9, 0.62],
        "absorption": [0.018, 0.023, 0.03],
        "phase_g": 0.58,
        "ior": 1.0,
        "parent": "glass_dome" if count >= 2 else None,
        "density": {"field": "main", "scale": 1.25, "bias": 0.02},
    }
    if count == 1:
        return [smoke]
    glass = {
        "id": "glass_dome",
        "shape": {"type": "sphere", "center": [0.0, 0.02, 0.0], "radius": 1.62},
        "scattering": [0.012, 0.016, 0.024],
        "absorption": [0.03, 0.022, 0.014],
        "phase_g": 0.42,
        "ior": 1.5,
    }
    dust = {
        "id": "dust_pocket",
        "shape": {"type": "sphere", "center": [0.48, 0.1, -0.04], "radius": 0.42},
        "scattering": [0.55, 0.78, 1.18],
        "absorption": [0.016, 0.025, 0.034],
        "phase_g": 0.35,
        "ior": 1.0,
        "parent": "glass_dome",
        "density": {"field": "main", "scale": 0.92, "bias": 0.0},
    }
    return [glass, smoke] if count == 2 else [glass, smoke, dust]


def build_sample(
    name: str,
    width: int,
    height: int,
    media_count: int,
    distribution: str,
    scene_id: str,
) -> tuple[bytes, dict[str, Any]]:
    field, payload = _density_field(distribution, sum(ord(char) for char in name))
    metadata = {
        "schema": 1,
        "scene_id": scene_id,
        "screen": {"width": width, "height": height},
        "camera": _camera(width, height),
        "ambient": [0.022, 0.031, 0.05],
        "light": {
            "direction": [0.48, 0.82, 0.31],
            "color": [1.0, 0.94, 0.82],
            "intensity": 5.5,
        },
        "density_fields": [field],
        "media": _media(media_count),
    }
    metadata_bytes = json.dumps(
        metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode("utf-8")
    payload_hash = sha256_bytes(payload)
    data = (
        MAGIC
        + struct.pack("<III", 1, len(metadata_bytes), len(payload))
        + bytes.fromhex(payload_hash)
        + metadata_bytes
        + payload
    )
    return data, metadata


def generate_sample_set(
    outdir: str | Path,
    seed: int = BASE_SEED,
    *,
    render_baselines: bool = True,
) -> dict[str, Any]:
    target = Path(outdir)
    target.mkdir(parents=True, exist_ok=True)
    specs = [
        ("smoke_small", 24, 18, 1, "gradient", seed + 11),
        ("nested_medium", 32, 24, 2, "wispy", seed + 29),
        ("triple_wide", 40, 30, 3, "uniform", seed + 47),
    ]
    samples: list[dict[str, Any]] = []
    base_config = {
        "chunk_width": 0,
        "chunk_height": 0,
        "parallelism": 1,
        "task_budget": 4_000_000,
        "budget_policy": "degrade",
        "base_step": 0.11,
        "min_step": 0.03,
        "max_step": 0.45,
        "density_adapt_gain": 1.25,
        "phase": 0.5,
        "shadow_samples": 3,
        "shadow_distance": 8.0,
        "precision": "float",
        "compare_precision": False,
        "seam_validation": False,
        "seam_tolerance": 2e-5,
    }
    for name, width, height, media_count, distribution, numeric_seed in specs:
        scene_id = f"{name}-{numeric_seed}"
        data, metadata = build_sample(
            name, width, height, media_count, distribution, scene_id
        )
        filename = f"{name}.dust"
        path = target / filename
        atomic_write_bytes(path, data)
        scene = Scene.from_bytes(data)
        config_dict = dict(base_config)
        config_dict["chunk_width"] = width
        config_dict["chunk_height"] = height
        config = RenderConfig.from_dict(config_dict)
        baseline: dict[str, Any] = {
            "config": config.to_dict(),
            "samples": 0,
            "image_sha256": "",
            "transmittance_sha256": "",
        }
        if render_baselines:
            run = RenderScheduler().render(scene, config)
            if run.error is not None:
                raise run.error
            baseline["samples"] = run.summary()["samples"]["total"]
            baseline["image_sha256"] = run.summary()["image_sha256"]
            baseline["transmittance_sha256"] = run.summary()["transmittance_sha256"]
        samples.append(
            {
                "name": name,
                "scene_id": scene_id,
                "file": filename,
                "width": width,
                "height": height,
                "medium_count": media_count,
                "density_distribution": distribution,
                "source_sha256": sha256_bytes(data),
                "payload_sha256": scene.payload_sha256,
                "density_version": scene.density_version,
                "density_blocks": [
                    list(block.index) for block in scene.density_fields["main"].blocks
                ],
                "baseline": baseline,
            }
        )
    manifest = {
        "format": "dustrender-sample-set",
        "format_version": 1,
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "samples": samples,
    }
    atomic_write_json(target / "manifest.json", manifest)
    return manifest
