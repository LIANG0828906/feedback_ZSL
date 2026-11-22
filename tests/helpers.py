from __future__ import annotations

import json
from typing import Any

import numpy as np

from volrender.config import RenderConfig, parse_config
from volrender.model import (
    MEDIUM_BOX,
    Camera,
    DensityField,
    DirectionalLight,
    Medium,
    Scene,
)


def config(**overrides: Any) -> RenderConfig:
    raw: dict[str, Any] = {
        "tile_size": 8,
        "max_parallelism": 2,
        "sample_budget": 500_000,
        "base_step": 0.18,
        "min_step": 0.012,
        "precision": "float",
        "degrade_sequence": [
            {"name": "S0_FULL", "step_multiplier": 1.0, "shadow_samples": 2},
            {"name": "S1_STEP_4X", "step_multiplier": 4.0, "shadow_samples": 2},
            {
                "name": "S2_STEP_12X_SHADOW_1",
                "step_multiplier": 12.0,
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
    raw.update(overrides)
    return parse_config(json.dumps(raw))


def camera(width: int, height: int, eye_z: float = -3.2) -> Camera:
    focal = 1.5 * width
    intrinsics = np.array(
        [
            [focal, 0.0, (width - 1) * 0.5],
            [0.0, focal, (height - 1) * 0.5],
            [0.0, 0.0, 1.0],
        ],
        dtype=np.float64,
    )
    view = np.eye(4, dtype=np.float64)
    view[2, 3] = -eye_z
    return Camera(intrinsics, view)


def density_field(
    field_id: int = 7,
    dims: tuple[int, int, int] = (6, 6, 6),
    chunk_shape: tuple[int, int, int] = (2, 2, 2),
    value_scale: float = 1.0,
) -> DensityField:
    nx, ny, nz = dims
    x = np.linspace(0.0, 1.0, nx)[None, None, :]
    y = np.linspace(0.0, 1.0, ny)[None, :, None]
    z = np.linspace(0.0, 1.0, nz)[:, None, None]
    full = value_scale * (
        0.5 + 0.9 * np.exp(-((x - 0.5) ** 2 + (y - 0.45) ** 2 + (z - 0.5) ** 2) / 0.12)
    )
    cx, cy, cz = chunk_shape
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
    return DensityField(field_id, dims, chunk_shape, chunks)


def box_medium(
    medium_id: int = 10,
    field_id: int = 7,
    lo: tuple[float, float, float] = (-0.85, -0.65, -0.55),
    hi: tuple[float, float, float] = (0.85, 0.65, 0.55),
    sigma_s: tuple[float, float, float] = (0.22, 0.17, 0.11),
    sigma_a: tuple[float, float, float] = (0.04, 0.035, 0.03),
) -> Medium:
    return Medium(
        id=medium_id,
        kind=MEDIUM_BOX,
        density_field_id=field_id,
        sigma_s=np.array(sigma_s, dtype=np.float64),
        sigma_a=np.array(sigma_a, dtype=np.float64),
        g=0.18,
        density_scale=1.0,
        shape_a=np.array(lo, dtype=np.float64),
        shape_b=np.array([hi[0], hi[1], hi[2], 0.0], dtype=np.float64),
    )


def tiny_scene(
    width: int = 24,
    height: int = 16,
    value_scale: float = 1.0,
) -> Scene:
    field = density_field(value_scale=value_scale)
    return Scene(
        width=width,
        height=height,
        camera=camera(width, height),
        light=DirectionalLight([0.1, 0.2, 1.0], [1.2, 1.0, 0.8]),
        media=[box_medium()],
        density_fields={7: field},
    )


def two_box_scene(width: int = 40, height: int = 30) -> Scene:
    left_field = density_field(field_id=7, value_scale=1.0)
    right_field = density_field(field_id=8, value_scale=1.2)
    left = box_medium(
        medium_id=10,
        field_id=7,
        lo=(-1.20, -0.50, -0.38),
        hi=(-0.35, 0.50, 0.38),
        sigma_s=(0.20, 0.14, 0.10),
    )
    right = box_medium(
        medium_id=11,
        field_id=8,
        lo=(0.35, -0.50, -0.38),
        hi=(1.20, 0.50, 0.38),
        sigma_s=(0.16, 0.20, 0.12),
    )
    return Scene(
        width=width,
        height=height,
        camera=camera(width, height, eye_z=-3.2),
        light=DirectionalLight([0.2, 0.25, 1.0], [1.1, 1.0, 0.9]),
        media=[left, right],
        density_fields={7: left_field, 8: right_field},
    )
