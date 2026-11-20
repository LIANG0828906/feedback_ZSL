#!/usr/bin/env python3
"""Generate the deterministic 64^3 density and segmentation fixtures."""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender.format import (  # noqa: E402
    FLAG_INTENSITY,
    FLAG_MASK,
    write_volume_file,
)


GENERATION_PARAMETERS = {
    "generator": "analytic-medical-phantom-v1",
    "shape_xyz": [64, 64, 64],
    "spacing_xyz": [1.0, 1.0, 1.0],
    "density_dtype": "int16",
    "mask_dtype": "uint8",
    "window_level": 120.0,
    "window_width": 260.0,
    "seed": "deterministic-no-random",
}


def generate_phantom(n: int = 64) -> tuple[np.ndarray, np.ndarray]:
    axis = np.linspace(-1.0, 1.0, n, dtype=np.float64)
    z, y, x = np.meshgrid(axis, axis, axis, indexing="ij")
    radius = np.sqrt(x * x + (0.92 * y) ** 2 + (0.78 * z) ** 2)

    body = radius < 0.86
    outer_shell = (radius >= 0.76) & (radius < 0.86)
    lesion = ((x - 0.25) ** 2 + (y + 0.12) ** 2 + (z - 0.10) ** 2) < 0.17**2
    cavity = ((x + 0.30) ** 2 + (y - 0.18) ** 2 + (z + 0.08) ** 2) < 0.14**2
    vessel = (
        (np.abs(x + 0.05) < 0.055)
        & (np.abs(y - 0.18 * np.sin(3.0 * z)) < 0.045)
        & (np.abs(z) < 0.65)
    )

    texture = (
        18.0 * np.sin(7.0 * x + 1.3 * z)
        + 11.0 * np.cos(9.0 * y - 2.0 * x)
        + 7.0 * np.sin(13.0 * z * y)
    )
    density = np.full((n, n, n), 8.0, dtype=np.float64)
    density += body * (95.0 + texture)
    density += outer_shell * 95.0
    density += vessel * 85.0
    density += lesion * 185.0
    density -= cavity * 80.0
    density += 2.0 * np.sin(19.0 * x + 17.0 * y + 23.0 * z)

    labels = np.zeros((n, n, n), dtype=np.uint8)
    labels[body] = 1
    labels[outer_shell] = 4
    labels[vessel] = 3
    labels[lesion] = 2
    labels[cavity] = 0
    return np.clip(np.rint(density), 0, 4095).astype(np.int16), labels


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", type=Path, default=ROOT / "samples")
    args = parser.parse_args()
    output = args.output_dir
    output.mkdir(parents=True, exist_ok=True)

    density, labels = generate_phantom()
    density_path = output / "density_64.vol"
    mask_path = output / "segmentation_64.vol"
    write_volume_file(
        density_path,
        density,
        spacing=(1.0, 1.0, 1.0),
        window_level=GENERATION_PARAMETERS["window_level"],
        window_width=GENERATION_PARAMETERS["window_width"],
        flags=FLAG_INTENSITY,
    )
    write_volume_file(
        mask_path,
        labels,
        spacing=(1.0, 1.0, 1.0),
        window_level=2.0,
        window_width=4.0,
        flags=FLAG_MASK,
    )

    density_values, density_counts = np.unique(density, return_counts=True)
    label_values, label_counts = np.unique(labels, return_counts=True)
    manifest = {
        "format": "VRVOL001-header72",
        "parameters": GENERATION_PARAMETERS,
        "files": {
            density_path.name: {
                "sha256": sha256_file(density_path),
                "bytes": density_path.stat().st_size,
                "dtype": str(density.dtype),
                "min": int(np.min(density)),
                "max": int(np.max(density)),
                "unique_values": int(density_values.size),
                "top_counts": {
                    str(int(value)): int(count)
                    for value, count in sorted(
                        zip(density_values, density_counts),
                        key=lambda pair: pair[1],
                        reverse=True,
                    )[:8]
                },
            },
            mask_path.name: {
                "sha256": sha256_file(mask_path),
                "bytes": mask_path.stat().st_size,
                "dtype": str(labels.dtype),
                "labels": {
                    str(int(value)): int(count)
                    for value, count in zip(label_values, label_counts)
                },
            },
        },
    }
    manifest_path = output / "manifest.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(manifest, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
