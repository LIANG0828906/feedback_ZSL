from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np


@dataclass(frozen=True)
class Band:
    index: int
    label: str
    center_hz: float
    low_hz: float
    high_hz: float

    def to_dict(self) -> dict[str, float | int | str]:
        return {
            "index": self.index,
            "label": self.label,
            "center_hz": self.center_hz,
            "low_hz": self.low_hz,
            "high_hz": self.high_hz,
        }


def _label(center: float) -> str:
    if center >= 1000.0:
        return f"{center / 1000.0:.2f}k"
    if center >= 100.0:
        return f"{center:.0f}"
    return f"{center:.1f}"


def make_bands(
    fraction: int, f_min_hz: float, f_max_hz: float
) -> tuple[Band, ...]:
    if fraction not in (1, 3):
        raise ValueError("fraction must be 1 or 3")
    step = 2.0 ** (1.0 / fraction)
    half_step = 2.0 ** (1.0 / (2.0 * fraction))
    min_exp = int(math.floor(fraction * math.log2(f_min_hz / 1000.0))) - 1
    max_exp = int(math.ceil(fraction * math.log2(f_max_hz / 1000.0))) + 1
    centers: list[float] = []
    for exponent in range(min_exp, max_exp + 1):
        center = 1000.0 * (step**exponent)
        if f_min_hz <= center <= f_max_hz:
            centers.append(center)
    bands: list[Band] = []
    for index, center in enumerate(sorted(set(centers))):
        low = max(f_min_hz, center / half_step)
        high = min(f_max_hz, center * half_step)
        if high <= low:
            continue
        bands.append(
            Band(
                index=index,
                label=_label(center),
                center_hz=float(center),
                low_hz=float(low),
                high_hz=float(high),
            )
        )
    return tuple(bands)


def band_weights(
    frequencies_hz: np.ndarray, band: Band
) -> tuple[np.ndarray, np.ndarray]:
    mask = (frequencies_hz >= band.low_hz) & (frequencies_hz < band.high_hz)
    indices = np.flatnonzero(mask)
    if indices.size == 0:
        nearest = int(np.argmin(np.abs(frequencies_hz - band.center_hz)))
        indices = np.asarray([nearest], dtype=np.int64)
    weights = np.ones(indices.size, dtype=np.float64)
    weights /= weights.sum()
    return indices, weights

