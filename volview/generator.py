"""Deterministic local volume generator.

Produces scalar volumes with a fixed seed.  Each volume declares its axis
order, spacing, gray calibration (slope/intercept) and slice counts, embeds
analytic geometry (sphere, cylindrical tube, rectangular block) at known
positions, and records the structures' centres, axis endpoints and radii as
ground truth in ``truth.json``.

The background is a smooth low-frequency field (repeated box filtering of
seeded noise) so that intersecting planes reconstructed from different
directions agree at the volume centre within the display tolerance.
"""

from __future__ import annotations

import json
import os
from typing import Dict, List

import numpy as np

DATASET_SEED = 20240517


def _smooth_noise(rng: np.random.Generator, shape, passes: int = 6) -> np.ndarray:
    """Seeded, smooth 3-D noise field in [-1, 1] (deterministic)."""
    field = rng.standard_normal(shape)
    for _ in range(passes):
        for axis in range(3):
            field = (np.roll(field, 1, axis=axis)
                     + field
                     + np.roll(field, -1, axis=axis)) / 3.0
    peak = float(np.max(np.abs(field)))
    return field / peak if peak > 0 else field


def _world_grids(shape, axis_order, spacing):
    """Per-voxel world coordinate grids (x, y, z) for the array."""
    indices = [np.arange(n, dtype=np.float64) for n in shape]
    grids = np.meshgrid(*indices, indexing="ij")
    world = {}
    for dim, letter in enumerate(axis_order):
        world[letter] = grids[dim] * spacing[letter]
    return world["X"], world["Y"], world["Z"]


def generate_volume(out_dir: str, volume_id: str, axis_order: str, shape,
                    spacing: Dict[str, float], slope: float, intercept: float,
                    gray_range, structures: List[dict],
                    background: float, noise_amplitude: float,
                    seed: int) -> dict:
    """Generate one volume directory; returns the truth record."""
    os.makedirs(out_dir, exist_ok=True)
    rng = np.random.default_rng(seed)
    field = background + noise_amplitude * _smooth_noise(rng, shape)
    gx, gy, gz = _world_grids(shape, axis_order, spacing)
    truth_structures = []
    for spec in structures:
        kind = spec["kind"]
        value = spec["value"]
        if kind == "sphere":
            center = spec["center"]
            radius = float(spec["radius"])
            mask = ((gx - center[0]) ** 2 + (gy - center[1]) ** 2
                    + (gz - center[2]) ** 2) <= radius ** 2
            field = np.where(mask, value, field)
            truth_structures.append({
                "kind": "sphere", "center": list(center), "radius": radius,
                "analytic_lengths": {"diameter": 2.0 * radius},
            })
        elif kind == "cylinder":
            p0 = np.asarray(spec["axis_start"], dtype=np.float64)
            p1 = np.asarray(spec["axis_end"], dtype=np.float64)
            radius = float(spec["radius"])
            axis = p1 - p0
            axis_len = float(np.linalg.norm(axis))
            direction = axis / axis_len
            coords = np.stack([gx, gy, gz], axis=-1)
            rel = coords - p0
            t = rel @ direction
            radial_sq = np.sum((rel - t[..., None] * direction) ** 2, axis=-1)
            mask = (t >= 0.0) & (t <= axis_len) & (radial_sq <= radius ** 2)
            field = np.where(mask, value, field)
            truth_structures.append({
                "kind": "cylinder",
                "axis_start": list(map(float, p0)),
                "axis_end": list(map(float, p1)),
                "radius": radius,
                "analytic_lengths": {
                    "axis_length": axis_len,
                    "diameter": 2.0 * radius,
                },
            })
        elif kind == "block":
            lo, hi = spec["corner_min"], spec["corner_max"]
            mask = ((gx >= lo[0]) & (gx <= hi[0])
                    & (gy >= lo[1]) & (gy <= hi[1])
                    & (gz >= lo[2]) & (gz <= hi[2]))
            field = np.where(mask, value, field)
            edges = [float(hi[i] - lo[i]) for i in range(3)]
            truth_structures.append({
                "kind": "block", "corner_min": list(lo),
                "corner_max": list(hi),
                "analytic_lengths": {"edge_x": edges[0], "edge_y": edges[1],
                                     "edge_z": edges[2]},
            })
        else:
            raise ValueError(f"unknown structure kind {kind!r}")

    raw = np.round((field - intercept) / slope).astype(np.int16)
    manifest = {
        "schema_version": 1,
        "volume_id": volume_id,
        "axis_order": axis_order,
        "shape": list(shape),
        "spacing": {a: float(spacing[a]) for a in ("X", "Y", "Z")},
        "slope": float(slope),
        "intercept": float(intercept),
        "gray_range": [float(gray_range[0]), float(gray_range[1])],
        "dtype": "int16",
        "data_file": "volume.npy",
    }
    np.save(os.path.join(out_dir, "volume.npy"), raw)
    with open(os.path.join(out_dir, "manifest.json"), "w",
              encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)
    truth = {"volume_id": volume_id, "seed": seed,
             "structures": truth_structures}
    with open(os.path.join(out_dir, "truth.json"), "w",
              encoding="utf-8") as fh:
        json.dump(truth, fh, indent=2, sort_keys=True)
    return truth


def generate_dataset(samples_dir: str, seed: int = DATASET_SEED) -> Dict[str, dict]:
    """Generate the full deterministic sample set; returns truths by id."""
    os.makedirs(samples_dir, exist_ok=True)
    truths: Dict[str, dict] = {}
    # vol_a: anisotropic spacing, ZYX storage order.
    truths["vol_a"] = generate_volume(
        os.path.join(samples_dir, "vol_a"), "vol_a",
        axis_order="ZYX", shape=(40, 64, 64),
        spacing={"X": 1.0, "Y": 1.0, "Z": 2.5},
        slope=1.0, intercept=-1024.0, gray_range=[-1024.0, 3071.0],
        background=300.0, noise_amplitude=25.0, seed=seed,
        structures=[
            {"kind": "sphere", "center": [32.0, 32.0, 50.0], "radius": 10.0,
             "value": 1200.0},
            {"kind": "cylinder", "axis_start": [12.0, 12.0, 20.0],
             "axis_end": [52.0, 12.0, 20.0], "radius": 4.0, "value": 900.0},
            {"kind": "block", "corner_min": [40.0, 44.0, 60.0],
             "corner_max": [56.0, 56.0, 80.0], "value": 1500.0},
        ])
    # vol_b: different axis order (XZY) and calibration to exercise mapping.
    truths["vol_b"] = generate_volume(
        os.path.join(samples_dir, "vol_b"), "vol_b",
        axis_order="XZY", shape=(48, 36, 48),
        spacing={"X": 0.9, "Y": 1.4, "Z": 0.9},
        slope=2.0, intercept=-1000.0, gray_range=[-1000.0, 3000.0],
        background=200.0, noise_amplitude=20.0, seed=seed + 1,
        structures=[
            {"kind": "sphere", "center": [21.6, 24.5, 21.6], "radius": 8.0,
             "value": 1100.0},
            {"kind": "cylinder", "axis_start": [9.0, 10.5, 9.0],
             "axis_end": [9.0, 38.5, 9.0], "radius": 3.5, "value": 800.0},
        ])
    # vol_c: small isotropic volume for fast checks.
    truths["vol_c"] = generate_volume(
        os.path.join(samples_dir, "vol_c"), "vol_c",
        axis_order="ZYX", shape=(24, 32, 32),
        spacing={"X": 1.5, "Y": 1.5, "Z": 1.5},
        slope=1.0, intercept=0.0, gray_range=[0.0, 4095.0],
        background=500.0, noise_amplitude=15.0, seed=seed + 2,
        structures=[
            {"kind": "sphere", "center": [24.0, 24.0, 18.0], "radius": 7.5,
             "value": 2000.0},
        ])
    return truths


if __name__ == "__main__":
    import sys
    out = sys.argv[1] if len(sys.argv) > 1 else "data/samples"
    generated = generate_dataset(out)
    for vid, truth in generated.items():
        kinds = [s["kind"] for s in truth["structures"]]
        print(f"generated {vid}: structures={kinds}")
