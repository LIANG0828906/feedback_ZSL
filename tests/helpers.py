from __future__ import annotations

from pathlib import Path

import numpy as np

from volrender.synthetic import make_synthetic_volume
from volrender.volume import VolumeData, load_volume, write_volume


def make_volume(directory: Path, size: int = 24, seed: int = 19093) -> VolumeData:
    density, mask, spec = make_synthetic_volume(size, seed)
    path = directory / f"test_{size}.vol"
    write_volume(path, density, spec.spacing, spec.window_center, spec.window_width, mask)
    return load_volume(path)


def small_uint16_volume(directory: Path) -> tuple[VolumeData, np.ndarray, np.ndarray]:
    density = np.arange(4 * 3 * 2, dtype=np.uint16).reshape(4, 3, 2) * 10
    mask = np.zeros_like(density, dtype=np.uint8)
    mask[1:3, 1:2, :] = 255
    path = directory / "tiny.vol"
    write_volume(path, density, (1.0, 1.25, 2.0), 30.0, 40.0, mask)
    return load_volume(path), density, mask
