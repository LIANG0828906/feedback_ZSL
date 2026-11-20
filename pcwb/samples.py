"""Deterministic local point-cloud sample generation.

All randomness comes from a seeded :class:`numpy.random.Generator`, so the
same seed always produces byte-identical ``.pclb`` files and baseline hashes.
Samples include structured surfaces, additive Gaussian noise and explicit
empty regions (spherical cavities) to exercise selection and operators.
"""

from __future__ import annotations

import json
import os

import numpy as np

from .format import write_cloud
from .versions import canonical_json


def generate_cloud(seed: int, n_surface: int, noise_ratio: float = 0.15,
                   cavity_radius: float = 1.2,
                   extent: float = 6.0) -> tuple[np.ndarray, np.ndarray]:
    """Build one deterministic sample cloud.

    A noisy three-plane corner structure with a spherical empty region
    (cavity) punched through its center.
    """
    rng = np.random.default_rng(seed)
    half = extent / 2.0

    # Structured points split across three orthogonal planes.
    per_plane = n_surface // 3
    uv = rng.uniform(-half, half, size=(per_plane, 2))
    plane_xy = np.column_stack([uv, np.full(per_plane, -half)])
    plane_xz = np.column_stack(
        [uv[:, 0], np.full(per_plane, -half), uv[:, 1]])
    plane_yz = np.column_stack(
        [np.full(per_plane, -half), uv[:, 0], uv[:, 1]])
    surface = np.concatenate([plane_xy, plane_xz, plane_yz], axis=0)

    n_noise = max(1, int(n_surface * noise_ratio))
    noise = rng.uniform(-half, half, size=(n_noise, 3))
    xyz = np.concatenate([surface, noise], axis=0)

    # Punch an explicit empty spherical region around the origin.
    keep = np.sum(xyz ** 2, axis=1) > cavity_radius ** 2
    xyz = xyz[keep]
    xyz += rng.normal(0.0, 0.03, size=xyz.shape)  # deterministic scan noise

    # Deterministic color field based on quantized position + RNG jitter.
    base = ((xyz - xyz.min(axis=0)) /
            np.maximum(xyz.max(axis=0) - xyz.min(axis=0), 1e-9))
    rgb = np.rint(base * 220.0 + 30.0).astype(np.uint8)
    jitter = rng.integers(-8, 9, size=rgb.shape)
    rgb = np.clip(rgb.astype(np.int16) + jitter, 0, 255).astype(np.uint8)

    order = np.lexsort((xyz[:, 0], xyz[:, 1], xyz[:, 2]))
    return np.ascontiguousarray(xyz[order]), np.ascontiguousarray(rgb[order])


def generate_sample_set(out_dir: str,
                        sizes=(512, 2048, 8192),
                        seed: int = 20261121) -> dict:
    """Generate the standard sample set plus a baseline hash manifest."""
    os.makedirs(out_dir, exist_ok=True)
    manifest = {"format": "pclb-samples", "seed": seed, "samples": []}
    for index, size in enumerate(sizes):
        xyz, rgb = generate_cloud(seed=seed + index, n_surface=size)
        name = f"sample_{size:06d}.pclb"
        path = os.path.join(out_dir, name)
        sha = write_cloud(path, xyz, rgb)
        manifest["samples"].append({
            "name": name,
            "seed": seed + index,
            "n_surface": int(size),
            "point_count": int(xyz.shape[0]),
            "sha256": sha,
        })
    manifest_path = os.path.join(out_dir, "samples_manifest.json")
    with open(manifest_path, "wb") as handle:
        handle.write(canonical_json(manifest))
    return manifest


def read_sample_manifest(out_dir: str) -> dict:
    with open(os.path.join(out_dir, "samples_manifest.json"), "rb") as h:
        return json.loads(h.read().decode("utf-8"))
