"""Deterministic local scene and configuration generator."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .binary import encode_scene


def _canonical_float(value: float) -> float:
    # Keep generated headers stable across platforms.
    return float(f"{value:.12g}")


def _camera(width: int, height: int) -> dict[str, Any]:
    f = _canonical_float(max(width, height) * 1.7)
    cx = _canonical_float(width / 2.0)
    cy = _canonical_float(height / 2.0)
    return {
        "width": width,
        "height": height,
        "K": [
            [f, 0.0, cx],
            [0.0, f, cy],
            [0.0, 0.0, 1.0],
        ],
        "world_to_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, 0.0],
            [0.0, 0.0, 1.0, 5.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
    }


def _smoke_values(dims: tuple[int, int, int]) -> np.ndarray:
    nx, ny, nz = dims
    values = np.empty((nz, ny, nx), dtype="<f4")
    for z in range(nz):
        for y in range(ny):
            for x in range(nx):
                fx = x / max(1, nx - 1)
                fy = y / max(1, ny - 1)
                fz = z / max(1, nz - 1)
                swirl = (
                    math.sin(2.7 * fx + 1.3 * fz)
                    + math.cos(3.4 * fy - 1.1 * fz)
                    + math.sin(2.1 * (fx + fy + fz))
                )
                density = 0.24 + 0.32 * (swirl + 3.0) / 6.0
                density *= 0.75 + 0.25 * math.sin(math.pi * fy)
                values[z, y, x] = np.float32(density)
    return values


def _glass_values(dims: tuple[int, int, int]) -> np.ndarray:
    nx, ny, nz = dims
    values = np.empty((nz, ny, nx), dtype="<f4")
    for z in range(nz):
        for y in range(ny):
            for x in range(nx):
                shell = 0.82 + 0.18 * (
                    (x + 2 * y + z) % 3
                ) / 2.0
                values[z, y, x] = np.float32(shell)
    return values


def build_sample(
    width: int = 48,
    height: int = 32,
    *,
    scene_id: str = "smoke_glass_demo",
) -> tuple[dict[str, Any], bytes]:
    smoke_dims = (6, 6, 6)
    glass_dims = (3, 3, 3)
    smoke_values = _smoke_values(smoke_dims)
    glass_values = _glass_values(glass_dims)
    smoke_count = int(np.prod(smoke_dims))
    glass_count = int(np.prod(glass_dims))
    payload = (
        np.ascontiguousarray(smoke_values, dtype="<f4").tobytes()
        + np.ascontiguousarray(glass_values, dtype="<f4").tobytes()
    )
    light = np.asarray([0.45, 0.82, -0.55], dtype=np.float64)
    light /= np.linalg.norm(light)
    header = {
        "format": "volrender-scene",
        "format_version": 1,
        "scene_id": scene_id,
        "camera": _camera(width, height),
        "light": {
            "direction": [_canonical_float(value) for value in light],
            "color": [1.0, 0.92, 0.80],
            "intensity": 6.0,
        },
        "background": [0.018, 0.024, 0.040],
        "media": [
            {
                "id": "glass_shell",
                "type": "sphere",
                "center": [0.0, 0.0, 2.45],
                "radius": 1.62,
                "density_block": 1,
                "sigma_s": 0.10,
                "sigma_a": 0.035,
                "phase_g": 0.15,
            },
            {
                "id": "smoke_plume",
                "type": "aabb",
                "min": [-0.55, -0.55, 1.72],
                "max": [0.55, 0.55, 3.18],
                "density_block": 0,
                "sigma_s": 0.95,
                "sigma_a": 0.18,
                "phase_g": 0.28,
            },
        ],
        "density_blocks": [
            {
                "block_id": 0,
                "medium_id": "smoke_plume",
                "dims": list(smoke_dims),
                "bounds": [-0.55, -0.55, 1.72, 0.55, 0.55, 3.18],
                "offset": 0,
                "count": smoke_count,
                "version": 1,
            },
            {
                "block_id": 1,
                "medium_id": "glass_shell",
                "dims": list(glass_dims),
                "bounds": [-1.62, -1.62, 0.83, 1.62, 1.62, 4.07],
                "offset": smoke_count,
                "count": glass_count,
                "version": 1,
            },
        ],
    }
    return header, payload


def default_config(
    *,
    sample_budget: int = 12000,
    tile_width: int = 16,
    tile_height: int = 16,
    max_workers: int = 2,
) -> dict[str, Any]:
    return {
        "tile_width": tile_width,
        "tile_height": tile_height,
        "max_workers": max_workers,
        "sample_budget": sample_budget,
        "base_step": 0.22,
        "shadow_samples": 4,
        "shadow_distance": 3.4,
        "max_distance": 14.0,
        "seam_tolerance": 1e-9,
        "degradation_sequence": [
            {"name": "full", "step_scale": 1.0},
            {"name": "balanced", "step_scale": 1.45},
            {"name": "draft", "step_scale": 2.05},
        ],
        "precision_paths": ["float64", "fixed"],
        "fixed_scale": 1 << 20,
    }


def write_sample(
    output_dir: str | Path,
    *,
    width: int = 48,
    height: int = 32,
    sample_budget: int = 12000,
    tile_width: int = 16,
    tile_height: int = 16,
    max_workers: int = 2,
) -> tuple[Path, Path]:
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    header, payload = build_sample(width=width, height=height)
    scene_path = out / "scene.volbin"
    config_path = out / "render_config.json"
    scene_path.write_bytes(encode_scene(header, payload))
    config_path.write_text(
        json.dumps(
            default_config(
                sample_budget=sample_budget,
                tile_width=tile_width,
                tile_height=tile_height,
                max_workers=max_workers,
            ),
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    return scene_path, config_path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="artifacts/sample")
    parser.add_argument("--width", type=int, default=48)
    parser.add_argument("--height", type=int, default=32)
    parser.add_argument("--sample-budget", type=int, default=18000)
    parser.add_argument("--tile-width", type=int, default=16)
    parser.add_argument("--tile-height", type=int, default=16)
    parser.add_argument("--max-workers", type=int, default=2)
    args = parser.parse_args(argv)
    scene_path, config_path = write_sample(
        args.out,
        width=args.width,
        height=args.height,
        sample_budget=args.sample_budget,
        tile_width=args.tile_width,
        tile_height=args.tile_height,
        max_workers=args.max_workers,
    )
    print(
        json.dumps(
            {
                "ok": True,
                "scene": str(scene_path),
                "config": str(config_path),
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
