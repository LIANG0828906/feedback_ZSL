"""Deterministic synthetic SVOL sample generation and baseline metadata."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .volume import build_header


SMALL = {
    "dims": (64, 64, 64),
    "spacing": (1.0, 1.0, 1.0),
    "baseline_step": 1.0,
    "baseline_image": 256,
}
LARGE = {
    "dims": (512, 512, 256),
    "spacing": (0.7, 0.7, 1.25),
    "baseline_step": 0.7,
    "baseline_image": 192,
}
WINDOW_WIDTH = 50000.0
WINDOW_LEVEL = 28000.0
DEFAULT_SEED = 20261121


def _coarse_noise(
    rng: np.random.Generator,
    dims: tuple[int, int, int],
) -> np.ndarray:
    depth, height, width = dims
    coarse_dims = (max(1, math.ceil(depth / 4)), max(1, math.ceil(height / 4)), max(1, math.ceil(width / 4)))
    noise = rng.standard_normal(coarse_dims).astype(np.float32)
    smoothed = (
        noise
        + np.roll(noise, 1, axis=0)
        + np.roll(noise, -1, axis=0)
        + np.roll(noise, 1, axis=1)
        + np.roll(noise, -1, axis=1)
    ) / np.float32(5.0)
    return smoothed


def synthesize(
    dims: tuple[int, int, int] = SMALL["dims"],
    *,
    seed: int = DEFAULT_SEED,
) -> tuple[np.ndarray, np.ndarray]:
    """Create a deterministic anatomy-like uint16 scalar and uint8 label mask."""

    depth, height, width = dims
    rng = np.random.default_rng(seed)
    coarse = _coarse_noise(rng, dims)
    scalar = np.empty(dims, dtype=np.uint16)
    mask = np.empty(dims, dtype=np.uint8)
    cx = (width - 1) * 0.5
    cy = (height - 1) * 0.5
    cz = (depth - 1) * 0.5
    x_axis = ((np.arange(width, dtype=np.float32) - cx) / (width * 0.46))[None, :]
    y_axis = ((np.arange(height, dtype=np.float32) - cy) / (height * 0.46))[:, None]
    lesion_a = (
        (width * 0.35, height * 0.38, depth * 0.56),
        (width * 0.09, height * 0.10, depth * 0.12),
    )
    lesion_b = (
        (width * 0.66, height * 0.61, depth * 0.42),
        (width * 0.07, height * 0.08, depth * 0.10),
    )
    for z in range(depth):
        zz = float((z - cz) / (depth * 0.47))
        radius_sq = x_axis * x_axis + y_axis * y_axis + np.float32(zz * zz)
        body = radius_sq < 1.0
        texture = (
            np.sin(x_axis * 23.0 + np.float32(z * 0.21))
            * np.cos(y_axis * 17.0 - np.float32(z * 0.13))
        )
        block = np.repeat(np.repeat(coarse[z // 4], 4, axis=0), 4, axis=1)[:height, :width]
        interior = (
            3000.0
            + 39000.0 * np.exp(-1.35 * radius_sq)
            + texture * 2400.0
            + block * 3200.0
        )
        values = np.where(body, interior, np.float32(3000.0))
        values = np.clip(values, 3000.0, 53000.0)
        scalar[z] = np.rint(values).astype(np.uint16)

        slice_mask = np.zeros((height, width), dtype=np.uint8)
        shell = body & (radius_sq > 0.78)
        slice_mask[shell] = 1
        for center, radii in (lesion_a, lesion_b):
            lx = (np.arange(width, dtype=np.float32) - center[0]) / radii[0]
            ly = (np.arange(height, dtype=np.float32) - center[1]) / radii[1]
            lz = (z - center[2]) / radii[2]
            lesion = lx[None, :] ** 2 + ly[:, None] ** 2 + lz**2 < 1.0
            slice_mask[lesion] = 2
        mask[z] = slice_mask
    return scalar, mask


def write_volume(
    path: str | Path,
    *,
    dims: tuple[int, int, int] = SMALL["dims"],
    spacing: tuple[float, float, float] = SMALL["spacing"],
    seed: int = DEFAULT_SEED,
    scalar: np.ndarray | None = None,
    mask: np.ndarray | None = None,
) -> dict[str, Any]:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    if scalar is None or mask is None:
        scalar, mask = synthesize(dims, seed=seed)
    if scalar.shape != dims or mask.shape != dims:
        raise ValueError("scalar/mask shape does not match dims")
    header = build_header(
        dims=dims,
        spacing=spacing,
        window_width=WINDOW_WIDTH,
        window_level=WINDOW_LEVEL,
        scalar_type_code=2,
        mask_type_code=1,
        scalar_reference_max=65535.0,
    )
    with target.open("wb") as handle:
        handle.write(header)
        np.asarray(scalar, dtype=np.dtype("<u2")).tofile(handle)
        np.asarray(mask, dtype=np.dtype("<u1")).tofile(handle)
    digest = hashlib.sha256()
    with target.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return {
        "dims": list(dims),
        "seed": seed,
        "spacing": list(spacing),
        "volume_sha256": digest.hexdigest(),
    }


def sample_specs() -> dict[str, dict[str, Any]]:
    return {"small": dict(SMALL), "large": dict(LARGE)}


def write_baseline_metadata(
    path: str | Path,
    *,
    tier: str,
    dims: tuple[int, int, int],
    spacing: tuple[float, float, float],
    seed: int,
    volume_sha256: str,
    normalization: dict[str, Any],
    step_size: float,
    image_size: int,
    content_hash: str,
) -> dict[str, Any]:
    payload = {
        "baseline_content_hash": content_hash,
        "baseline_image": [image_size, image_size],
        "baseline_step_size": step_size,
        "dims": list(dims),
        "normalization": {
            "mask": {
                "max_label": normalization["mask"]["max_label"],
                "source": normalization["mask"]["source"],
                "truth_source": normalization["mask"]["truth_source"],
            },
            "scalar": {
                "scalar_reference_max": normalization["scalar"]["reference_max"],
                "source": normalization["scalar"]["source"],
                "truth_source": normalization["scalar"]["truth_source"],
                "window_high": normalization["scalar"]["window_high"],
                "window_low": normalization["scalar"]["window_low"],
            },
        },
        "seed": seed,
        "spacing": list(spacing),
        "tier": tier,
        "volume_sha256": volume_sha256,
        "window": [WINDOW_WIDTH, WINDOW_LEVEL],
    }
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return payload


__all__ = [
    "DEFAULT_SEED",
    "LARGE",
    "SMALL",
    "WINDOW_LEVEL",
    "WINDOW_WIDTH",
    "sample_specs",
    "synthesize",
    "write_baseline_metadata",
    "write_volume",
]

