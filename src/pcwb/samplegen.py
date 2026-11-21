"""Deterministic synthetic scan generation."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .format import serialize_point_cloud, write_point_cloud
from .model import PointCloud
from .util import pretty_json_bytes, sha256_hex


@dataclass(frozen=True, slots=True)
class SampleSpec:
    name: str
    filename: str
    count: int
    extent: float
    hole_radius: float
    noise: float
    seed_offset: int


DEFAULT_SPECS = (
    SampleSpec("small-noisy", "small-noisy.pcwb", 2400, 2.0, 0.28, 0.025, 1),
    SampleSpec("medium-hole", "medium-hole.pcwb", 12000, 3.2, 0.52, 0.035, 2),
    SampleSpec("large-sparse", "large-sparse.pcwb", 40000, 5.0, 0.75, 0.045, 3),
)


def _generate_arrays(spec: SampleSpec, seed: int) -> tuple[np.ndarray, np.ndarray]:
    rng = np.random.Generator(np.random.PCG64(seed + spec.seed_offset))
    accepted_xyz: list[np.ndarray] = []
    accepted_rgb: list[np.ndarray] = []
    total = 0
    while total < spec.count:
        batch = max(1024, min(spec.count - total, 65536))
        xy = rng.uniform(-spec.extent, spec.extent, size=(batch, 2))
        radius = np.sqrt(np.einsum("ij,ij->i", xy, xy))
        # Circular scan seam plus a deliberate empty interior region.
        outer = radius <= spec.extent
        hole = radius >= spec.hole_radius
        keep = outer & hole
        if not keep.any():
            continue
        xy = xy[keep]
        x = xy[:, 0]
        y = xy[:, 1]
        z = (
            0.30 * np.sin(1.35 * x)
            + 0.18 * np.cos(1.10 * y)
            + 0.07 * np.sin(2.8 * (x + y))
        )
        xyz = np.column_stack(
            [
                x,
                y,
                z + rng.normal(0.0, spec.noise, size=x.shape[0]),
            ]
        ).astype(np.float32)
        radial = np.sqrt((x * x + y * y) / (spec.extent * spec.extent))
        red = np.clip(
            48 + 165 * (0.5 + 0.5 * np.sin(1.8 * x + 0.3 * spec.seed_offset)),
            0,
            255,
        )
        green = np.clip(55 + 150 * (0.5 + 0.5 * np.cos(1.3 * y)), 0, 255)
        blue = np.clip(45 + 175 * (1.0 - 0.7 * radial), 0, 255)
        rgb = np.column_stack([red, green, blue]).astype(np.uint8)
        accepted_xyz.append(xyz)
        accepted_rgb.append(rgb)
        total += xyz.shape[0]

    xyz_all = np.concatenate(accepted_xyz, axis=0)[: spec.count]
    rgb_all = np.concatenate(accepted_rgb, axis=0)[: spec.count]
    return np.ascontiguousarray(xyz_all, dtype=np.float32), np.ascontiguousarray(
        rgb_all, dtype=np.uint8
    )


def generate_cloud(spec: SampleSpec, *, seed: int) -> PointCloud:
    xyz, rgb = _generate_arrays(spec, seed)
    return PointCloud.from_arrays(xyz, rgb)


def generate_samples(
    output_dir: str | Path,
    *,
    seed: int = 20261122,
    specs: tuple[SampleSpec, ...] = DEFAULT_SPECS,
) -> dict[str, Any]:
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    records: list[dict[str, Any]] = []
    for spec in specs:
        cloud = generate_cloud(spec, seed=seed)
        target = out / spec.filename
        digest = write_point_cloud(target, cloud, atomic=True)
        payload = serialize_point_cloud(cloud)
        records.append(
            {
                "name": spec.name,
                "file": spec.filename,
                "count": cloud.count,
                "bounds": cloud.bounds.to_dict(),
                "sha256": digest,
                "bytes": len(payload),
            }
        )
    manifest = {
        "schema": "pcwb-samples-v1",
        "seed": int(seed),
        "samples": records,
    }
    baseline_path = out / "baselines.json"
    baseline_payload = pretty_json_bytes(manifest)
    # write atomically through a temporary file
    temporary = out / ".baselines.json.tmp"
    temporary.write_bytes(baseline_payload)
    temporary.replace(baseline_path)
    manifest["baseline_path"] = str(baseline_path)
    manifest["baseline_sha256"] = sha256_hex(baseline_payload)
    return manifest
