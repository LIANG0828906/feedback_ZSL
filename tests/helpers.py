from __future__ import annotations

import numpy as np

from volrender.transfer import TransferFunction
from volrender.volume import VolumeData


def synthetic_volume(n: int = 24) -> VolumeData:
    axis = np.linspace(-1.0, 1.0, n, dtype=np.float64)
    z, y, x = np.meshgrid(axis, axis, axis, indexing="ij")
    sphere = x * x + y * y + z * z < 0.72**2
    inner = (x - 0.20) ** 2 + (y + 0.10) ** 2 + (z - 0.10) ** 2 < 0.28**2
    raw = np.where(sphere, 120.0, 0.0) + np.where(inner, 180.0, 0.0)
    raw += 12.0 * np.sin(5.0 * x + 7.0 * y)
    return VolumeData(
        np.rint(raw).astype(np.int16),
        spacing=(1.0, 1.0, 1.0),
        window_level=140.0,
        window_width=320.0,
        truth_source="synthetic_sphere",
    )


def synthetic_transfer() -> TransferFunction:
    return TransferFunction(
        [
            (0.0, 0.0, 0.0, 0.0, 0.0),
            (0.15, 0.02, 0.2, 0.5, 1.0),
            (0.48, 0.18, 1.0, 0.7, 0.2),
            (0.76, 0.62, 1.0, 0.9, 0.8),
            (1.0, 0.95, 1.0, 1.0, 1.0),
        ]
    )
