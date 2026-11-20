from __future__ import annotations

import math

import numpy as np

from .errors import CalibrationError


def choose_nfft(taps: int) -> int:
    target = max(16384, taps * 16)
    nfft = 1 << int(math.ceil(math.log2(target)))
    return min(nfft, 262144)


def correction_segments(centers_hz: np.ndarray, corrections_db: np.ndarray) -> list[dict[str, object]]:
    segments: list[dict[str, object]] = []
    for index in range(len(centers_hz) - 1):
        segments.append(
            {
                "kind": "between",
                "lower_center_hz": float(centers_hz[index]),
                "upper_center_hz": float(centers_hz[index + 1]),
                "lower_correction_db": float(corrections_db[index]),
                "upper_correction_db": float(corrections_db[index + 1]),
            }
        )
    segments.insert(
        0,
        {
            "kind": "below",
            "lower_center_hz": 0.0,
            "upper_center_hz": float(centers_hz[0]),
            "lower_correction_db": float(corrections_db[0]),
            "upper_correction_db": float(corrections_db[0]),
        },
    )
    segments.append(
        {
            "kind": "above",
            "lower_center_hz": float(centers_hz[-1]),
            "upper_center_hz": 1e9,
            "lower_correction_db": float(corrections_db[-1]),
            "upper_correction_db": float(corrections_db[-1]),
        }
    )
    return segments


def corrections_on_grid(
    frequencies_hz: np.ndarray,
    centers_hz: np.ndarray,
    corrections_db: np.ndarray,
) -> np.ndarray:
    if centers_hz.size == 1:
        return np.full(frequencies_hz.size, corrections_db[0], dtype=np.float64)
    safe = np.maximum(frequencies_hz, 1e-9)
    log_f = np.log2(safe)
    log_centers = np.log2(centers_hz)
    return np.interp(log_f, log_centers, corrections_db)


def design_linear_phase_fir(
    centers_hz: np.ndarray,
    corrections_db: np.ndarray,
    taps: int,
    sample_rate: int,
    nfft: int,
) -> dict[str, object]:
    if not np.all(np.isfinite(corrections_db)):
        raise CalibrationError(
            "E_FILTER_INVALID", "non-finite compensation correction", "target_curve"
        )
    frequencies = np.fft.rfftfreq(nfft, d=1.0 / sample_rate)
    correction_grid = corrections_on_grid(frequencies, centers_hz, corrections_db)
    desired = np.power(10.0, correction_grid / 20.0)
    if not np.all(np.isfinite(desired)):
        raise CalibrationError(
            "E_FILTER_INVALID", "invalid desired filter response", "target_curve"
        )
    zero_phase = np.fft.irfft(desired, n=nfft)
    centered = np.fft.fftshift(zero_phase)
    midpoint = nfft // 2
    half = taps // 2
    coefficients = centered[midpoint - half : midpoint + half + 1].copy()
    coefficients *= np.blackman(taps)
    if not np.all(np.isfinite(coefficients)):
        raise CalibrationError(
            "E_FILTER_INVALID", "FIR design produced non-finite coefficients", "filter_taps"
        )

    response_frequencies = np.geomspace(20.0, min(20000.0, sample_rate / 2.0), 320)
    index = np.arange(taps, dtype=np.float64)
    response = np.zeros(response_frequencies.size, dtype=np.complex128)
    for start in range(0, response_frequencies.size, 32):
        stop = min(start + 32, response_frequencies.size)
        phase = np.exp(
            -2j
            * np.pi
            * response_frequencies[start:stop, None]
            * index[None, :]
            / sample_rate
        )
        response[start:stop] = phase @ coefficients
    response_db = 20.0 * np.log10(np.maximum(np.abs(response), 1e-15))

    band_phase = np.exp(
        -2j * np.pi * centers_hz[:, None] * index[None, :] / sample_rate
    )
    band_response = band_phase @ coefficients
    realized_db = 20.0 * np.log10(np.maximum(np.abs(band_response), 1e-15))
    truncation_db = np.abs(realized_db - corrections_db)

    return {
        "coefficients": coefficients.astype(np.float64),
        "group_delay_samples": float((taps - 1) / 2.0),
        "group_delay_seconds": float((taps - 1) / (2.0 * sample_rate)),
        "nfft": int(nfft),
        "response_frequencies_hz": response_frequencies.astype(np.float64),
        "response_db": response_db.astype(np.float64),
        "band_realized_db": realized_db.astype(np.float64),
        "truncation_db_bound": truncation_db.astype(np.float64),
    }

