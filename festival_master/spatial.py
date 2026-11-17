from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np


SPATIAL_STABILITY_THRESHOLD_DB = 0.01
PAN_LIMIT = 0.98
MAX_ITD_SECONDS = 0.00065


@dataclass(frozen=True)
class PanParameters:
    azimuth_deg: float
    elevation_deg: float
    pan: float
    left_gain: float
    right_gain: float
    ild_db: float
    itd_seconds: float
    left_delay_samples: float
    right_delay_samples: float
    max_ild_step_db: float


def source_angles(
    anchor: tuple[float, float, float],
    listener: tuple[float, float, float],
) -> tuple[float, float]:
    dx = float(anchor[0]) - float(listener[0])
    dy = float(anchor[1]) - float(listener[1])
    dz = float(anchor[2]) - float(listener[2])
    horizontal = math.hypot(dx, dy)
    azimuth = math.degrees(math.atan2(dx, dy))
    elevation = math.degrees(math.atan2(dz, horizontal))
    return azimuth, elevation


def pan_parameters(azimuth_deg: float, sample_rate: int) -> PanParameters:
    azimuth = max(-90.0, min(90.0, float(azimuth_deg)))
    pan = max(-PAN_LIMIT, min(PAN_LIMIT, azimuth / 90.0))
    left_gain = math.sqrt(0.5 * (1.0 - pan))
    right_gain = math.sqrt(0.5 * (1.0 + pan))
    ild_db = 20.0 * math.log10(max(left_gain, 1e-12) / max(right_gain, 1e-12))

    # A cue-sheet anchor is fixed for the whole performance, so the ILD control
    # value is constant at every adjacent sample pair.
    max_ild_step = 0.0

    itd_seconds = MAX_ITD_SECONDS * math.sin(math.radians(azimuth))
    left_delay = max(itd_seconds, 0.0) * sample_rate
    right_delay = max(-itd_seconds, 0.0) * sample_rate
    return PanParameters(
        azimuth_deg=azimuth,
        elevation_deg=0.0,
        pan=pan,
        left_gain=left_gain,
        right_gain=right_gain,
        ild_db=ild_db,
        itd_seconds=itd_seconds,
        left_delay_samples=left_delay,
        right_delay_samples=right_delay,
        max_ild_step_db=max_ild_step,
    )


def fractional_delay(samples: np.ndarray, delay_samples: float) -> np.ndarray:
    """Causal linear-interpolation delay with unchanged output length."""
    values = np.asarray(samples, dtype=np.float64)
    if delay_samples <= 0.0:
        return values.copy()
    positions = np.arange(values.size, dtype=np.float64) - delay_samples
    lower = np.floor(positions).astype(np.int64)
    fraction = positions - lower
    valid_lower = (lower >= 0) & (lower < values.size)
    valid_upper = (lower + 1 >= 0) & (lower + 1 < values.size)
    lower_values = np.where(valid_lower, values[np.clip(lower, 0, values.size - 1)], 0.0)
    upper_values = np.where(valid_upper, values[np.clip(lower + 1, 0, values.size - 1)], 0.0)
    return lower_values * (1.0 - fraction) + upper_values * fraction


def render_binaural(
    mono: np.ndarray,
    pan: PanParameters,
) -> tuple[np.ndarray, np.ndarray]:
    values = np.asarray(mono, dtype=np.float64)
    left = fractional_delay(values, pan.left_delay_samples) * pan.left_gain
    right = fractional_delay(values, pan.right_delay_samples) * pan.right_gain
    return left, right


def energy_percentages(left: np.ndarray, right: np.ndarray) -> tuple[float, float]:
    left_energy = float(np.sum(np.asarray(left, dtype=np.float64) ** 2))
    right_energy = float(np.sum(np.asarray(right, dtype=np.float64) ** 2))
    total = left_energy + right_energy
    if total <= 1e-30:
        return 50.0, 50.0
    return 100.0 * left_energy / total, 100.0 * right_energy / total
