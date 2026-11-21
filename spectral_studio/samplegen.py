"""Deterministic local sample generator.

Generates local material samples (multiple wavelength grids, spectral
curvatures and target color spaces) plus a baseline file of content
hashes.  Pure function of the seed: no network, accounts or external
datasets.  Re-running with the same seed regenerates byte-identical
samples.
"""

from __future__ import annotations

import argparse
import json
import os

import numpy as np

from .canon import canonical_json, content_hash

DEFAULT_SEED = 20240117


def _smooth_curve(rng, wavelengths, n_harmonics=3):
    """Seeded smooth reflectance curve in [0.05, 0.95]."""
    curve = np.full_like(wavelengths, 0.5, dtype=np.float64)
    span = wavelengths[-1] - wavelengths[0]
    for _ in range(n_harmonics):
        freq = rng.uniform(0.5, 3.0) / span * 2.0 * np.pi
        phase = rng.uniform(0.0, 2.0 * np.pi)
        amp = rng.uniform(0.08, 0.3)
        curve = curve + amp * np.sin(freq * (wavelengths - wavelengths[0]) + phase)
    return np.clip(curve, 0.05, 0.95)


def generate_samples(seed=DEFAULT_SEED):
    """Return (materials, baseline) -- deterministic in *seed*."""
    rng = np.random.default_rng(seed)
    materials = []

    grids = [
        (400.0, 700.0, 10.0),
        (380.0, 780.0, 5.0),
        (420.0, 680.0, 4.0),
    ]
    for index, (start, end, step) in enumerate(grids):
        count = int(round((end - start) / step)) + 1
        wavelengths = start + step * np.arange(count)
        curve = _smooth_curve(rng, wavelengths, n_harmonics=2 + index)
        materials.append({
            "id": f"spectral-{index + 1}",
            "kind": "spectral",
            "spectral": {
                "wavelengths_nm": [float(v) for v in wavelengths],
                "interval_nm": float(step),
                "reflectance": [float(v) for v in curve],
            },
        })

    srgb_colors = [[0.8, 0.2, 0.2], [0.2, 0.7, 0.3]]
    for index, rgb in enumerate(srgb_colors):
        materials.append({"id": f"srgb-{index + 1}", "kind": "srgb",
                          "rgb": list(rgb)})

    p3_colors = [[0.9, 0.5, 0.1], [0.1, 0.4, 0.9]]
    for index, rgb in enumerate(p3_colors):
        materials.append({"id": f"p3-{index + 1}", "kind": "display-p3",
                          "rgb": list(rgb)})

    baseline = {
        "seed": seed,
        "material_hashes": {m["id"]: content_hash(m) for m in materials},
    }
    return materials, baseline


def write_samples(directory, seed=DEFAULT_SEED):
    """Write samples + baseline into *directory*; returns file paths."""
    os.makedirs(directory, exist_ok=True)
    materials, baseline = generate_samples(seed)
    paths = []
    materials_dir = os.path.join(directory, "materials")
    os.makedirs(materials_dir, exist_ok=True)
    for material in materials:
        path = os.path.join(materials_dir, f"{material['id']}.json")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(canonical_json(material))
        paths.append(path)
    baseline_path = os.path.join(directory, "baseline_hashes.json")
    with open(baseline_path, "w", encoding="utf-8") as handle:
        handle.write(canonical_json(baseline))
    paths.append(baseline_path)
    return paths


def load_samples(directory):
    materials_dir = os.path.join(directory, "materials")
    materials = []
    for name in sorted(os.listdir(materials_dir)):
        if name.endswith(".json"):
            with open(os.path.join(materials_dir, name), "r",
                      encoding="utf-8") as handle:
                materials.append(json.load(handle))
    return materials


def main(argv=None):
    parser = argparse.ArgumentParser(description="deterministic sample generator")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--out", default="samples")
    args = parser.parse_args(argv)
    paths = write_samples(args.out, seed=args.seed)
    for path in paths:
        print(path)


if __name__ == "__main__":
    main()
