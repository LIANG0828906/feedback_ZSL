from __future__ import annotations

import numpy as np


PQ_M1 = 2610.0 / 16384.0
PQ_M2 = 2523.0 / 4096.0 * 128.0
PQ_C1 = 3424.0 / 4096.0
PQ_C2 = 2413.0 / 4096.0 * 32.0
PQ_C3 = 2392.0 / 4096.0 * 32.0


def srgb_decode(values: np.ndarray) -> np.ndarray:
    values = np.asarray(values, dtype=np.float64)
    threshold = 0.04045
    low = values / 12.92
    high = np.power((np.abs(values) + 0.055) / 1.055, 2.4)
    return np.where(values <= threshold, low, np.copysign(high, values))


def srgb_encode(values: np.ndarray) -> np.ndarray:
    values = np.asarray(values, dtype=np.float64)
    threshold = 0.0031308
    low = values * 12.92
    high = 1.055 * np.power(np.abs(values), 1.0 / 2.4) - 0.055
    return np.where(values <= threshold, low, np.copysign(high, values))


def pq_decode(values: np.ndarray) -> np.ndarray:
    """ST 2084 code value/10000 to normalized absolute luminance 0..1."""

    values = np.asarray(values, dtype=np.float64)
    safe = np.clip(values, 0.0, 1.0)
    powered = np.power(safe, 1.0 / PQ_M2)
    numerator = np.maximum(powered - PQ_C1, 0.0)
    denominator = PQ_C2 - PQ_C3 * powered
    return np.power(numerator / denominator, 1.0 / PQ_M1)


def pq_encode(values: np.ndarray) -> np.ndarray:
    """Normalized absolute luminance 0..1 to ST 2084 code value."""

    values = np.asarray(values, dtype=np.float64)
    safe = np.clip(values, 0.0, 1.0)
    powered = np.power(safe, PQ_M1)
    numerator = PQ_C1 + PQ_C2 * powered
    denominator = 1.0 + PQ_C3 * powered
    return np.power(numerator / denominator, PQ_M2)


def decode_transfer(values: np.ndarray, transfer: str, peak_nits: float) -> np.ndarray:
    if transfer == "linear":
        return np.asarray(values, dtype=np.float64)
    if transfer == "srgb":
        return srgb_decode(values)
    if transfer == "pq":
        return pq_decode(values) * (10000.0 / float(peak_nits))
    raise ValueError(f"unsupported transfer function: {transfer}")


def encode_transfer(values: np.ndarray, transfer: str, peak_nits: float) -> np.ndarray:
    if transfer == "linear":
        return np.asarray(values, dtype=np.float64)
    if transfer == "srgb":
        return srgb_encode(values)
    if transfer == "pq":
        return pq_encode(np.asarray(values, dtype=np.float64) * (float(peak_nits) / 10000.0))
    raise ValueError(f"unsupported transfer function: {transfer}")
