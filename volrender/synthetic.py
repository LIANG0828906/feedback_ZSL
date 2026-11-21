"""Deterministic local synthetic scalar-volume generator."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .params import default_view_matrix, make_parameters
from .transfer import DEFAULT_CONTROL_POINTS
from .volume import write_volume

DEFAULT_SEED = 19093
DEFAULT_LEVELS = (64, 128)


@dataclass(frozen=True)
class SyntheticSpec:
    size: int
    spacing: tuple[float, float, float]
    window_center: float
    window_width: float
    seed: int


def _level_spec(size: int, seed: int) -> SyntheticSpec:
    if size == 64:
        return SyntheticSpec(size, (1.0, 1.0, 1.0), 1700.0, 2500.0, seed)
    if size == 128:
        return SyntheticSpec(size, (0.75, 0.75, 1.0), 1750.0, 2700.0, seed)
    spacing = (64.0 / float(size), 64.0 / float(size), 64.0 / float(size))
    return SyntheticSpec(size, spacing, 1700.0, 2500.0, seed)


def make_synthetic_volume(size: int, seed: int = DEFAULT_SEED) -> tuple[np.ndarray, np.ndarray, SyntheticSpec]:
    if size < 2:
        raise ValueError("synthetic volume size must be at least 2")
    spec = _level_spec(int(size), int(seed))
    rng = np.random.default_rng(spec.seed)
    axis = (np.arange(size, dtype=np.float64) + 0.5) / size
    x, y, z = np.meshgrid(axis, axis, axis, indexing="ij")
    x = (x - 0.5) * 2.0
    y = (y - 0.5) * 2.0
    z = (z - 0.5) * 2.0

    body = np.exp(-(x * x / 0.72 + y * y / 0.48 + z * z / 0.62) * 2.0)
    core = np.exp(-(((x - 0.10) ** 2) / 0.055 + ((y + 0.05) ** 2) / 0.045 + (z * z) / 0.06))
    shell_radius = np.sqrt((x * x) / 0.60 + (y * y) / 0.38 + (z * z) / 0.50)
    shell = np.exp(-((shell_radius - 0.72) ** 2) / 0.004)
    lesion_a = np.exp(-(((x + 0.48) ** 2) + ((y - 0.32) ** 2) + ((z - 0.18) ** 2)) / 0.012)
    lesion_b = np.exp(-(((x - 0.46) ** 2) + ((y + 0.42) ** 2) + ((z + 0.32) ** 2)) / 0.018)

    pattern = 0.5 + 0.5 * np.sin(14.0 * x + 9.0 * y - 7.0 * z)
    density = (
        680.0
        + 1320.0 * body
        + 980.0 * core
        + 760.0 * shell
        + 2350.0 * lesion_a
        + 1850.0 * lesion_b
        + 260.0 * pattern * body
        + rng.normal(0.0, 18.0, size=(size, size, size))
    )
    density = np.clip(np.rint(density), 0.0, 4095.0).astype(np.uint16)

    mask_sphere = ((x - 0.08) ** 2 / 0.055 + (y + 0.04) ** 2 / 0.045 + z * z / 0.060) <= 1.0
    mask_shell = (shell_radius >= 0.68) & (shell_radius <= 0.76) & (y > -0.15)
    mask_lesions = (lesion_a > 0.45) | (lesion_b > 0.40)
    mask = (mask_sphere | mask_shell | mask_lesions).astype(np.uint8) * 255
    return density, mask, spec


def baseline_parameters(volume, *, viewport: tuple[int, int] = (192, 192)):
    return make_parameters(
        volume_hash=volume.content_sha256,
        window_center=volume.header.window_center,
        window_width=volume.header.window_width,
        control_points=DEFAULT_CONTROL_POINTS,
        step_size=0.012,
        view_matrix=default_view_matrix(),
        viewport=viewport,
        mask_enabled=volume.has_mask,
        mask_hash=volume.mask_sha256,
    )


def generate_sample(
    output_directory: str | os.PathLike[str],
    size: int = 64,
    seed: int = DEFAULT_SEED,
) -> dict[str, object]:
    from .volume import load_volume

    root = Path(output_directory)
    root.mkdir(parents=True, exist_ok=True)
    density, mask, spec = make_synthetic_volume(size, seed)
    name = f"synthetic_{size}x{size}x{size}"
    volume_path = root / f"{name}.vol"
    write_volume(
        volume_path,
        density,
        spec.spacing,
        spec.window_center,
        spec.window_width,
        mask,
        scalar_type="uint16",
    )
    volume = load_volume(volume_path)
    params = baseline_parameters(volume)
    record: dict[str, object] = {
        "name": name,
        "path": volume_path.name,
        "seed": spec.seed,
        "dims": list(volume.dims),
        "spacing": list(volume.spacing),
        "scalar_type": volume.header.scalar_type,
        "window_center": spec.window_center,
        "window_width": spec.window_width,
        "mask_offset": volume.header.mask_offset,
        "mask_pixels": int(np.count_nonzero(volume.mask)),
        "density_range": [int(volume.density.min()), int(volume.density.max())],
        "density_sha256": volume.density_sha256,
        "mask_sha256": volume.mask_sha256,
        "file_sha256": volume.file_sha256,
        "content_sha256": volume.content_sha256,
        "baseline_parameter_hash": params.parameter_hash,
        "baseline_parameters": params.to_snapshot_dict(include_hash=False),
    }
    metadata_path = root / f"{name}.params.json"
    metadata_path.write_text(
        json.dumps(record, ensure_ascii=True, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    return record


def generate_samples(
    output_directory: str | os.PathLike[str],
    *,
    sizes: tuple[int, ...] = DEFAULT_LEVELS,
    seed: int = DEFAULT_SEED,
) -> list[dict[str, object]]:
    root = Path(output_directory)
    records = [generate_sample(root, size=size, seed=seed) for size in sizes]
    manifest = {
        "generator": "volrender.synthetic",
        "generator_version": 1,
        "seed": seed,
        "levels": records,
    }
    (root / "samples.json").write_text(
        json.dumps(manifest, ensure_ascii=True, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    return records
