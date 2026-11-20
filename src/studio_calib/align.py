from __future__ import annotations

import numpy as np

from .errors import CalibrationError


def robust_noise_sigma(samples: np.ndarray) -> float:
    """MAD estimate from the pre-arrival prefix, which need not be pure noise."""
    prefix_length = min(128, samples.size)
    prefix = samples[:prefix_length]
    if prefix.size == 0:
        return 0.0
    median = float(np.median(prefix))
    mad = float(np.median(np.abs(prefix - median)))
    return 1.4826 * mad


def detect_arrival(
    samples: np.ndarray,
    threshold_sigma: float,
    threshold_peak_ratio: float,
) -> tuple[int, float, float, float]:
    if samples.size == 0:
        raise CalibrationError("E_ALIGNMENT", "empty channel")
    magnitude = np.abs(samples)
    peak = float(np.max(magnitude))
    noise_sigma = robust_noise_sigma(samples)
    threshold = max(
        noise_sigma * threshold_sigma,
        peak * threshold_peak_ratio,
        np.finfo(np.float64).tiny,
    )
    crossings = np.flatnonzero(magnitude > threshold)
    if crossings.size == 0:
        raise CalibrationError("E_ALIGNMENT", "direct-sound threshold was never crossed")
    arrival = int(crossings[0])
    direct_end = min(samples.size, arrival + 128)
    direct_peak = float(np.max(magnitude[arrival:direct_end]))
    if direct_peak <= 0.0:
        raise CalibrationError("E_ALIGNMENT", "direct-sound peak is zero")
    return arrival, threshold, direct_peak, noise_sigma


def align_channel(
    samples: np.ndarray,
    reference_samples: np.ndarray,
    threshold_sigma: float,
    threshold_peak_ratio: float,
) -> dict[str, object]:
    arrival, threshold, peak, noise_sigma = detect_arrival(
        samples, threshold_sigma, threshold_peak_ratio
    )
    ref_arrival, ref_threshold, ref_peak, ref_noise_sigma = detect_arrival(
        reference_samples, threshold_sigma, threshold_peak_ratio
    )
    offset = int(arrival - ref_arrival)
    normalization_gain = float(ref_peak / peak)

    aligned = np.zeros_like(samples)
    if offset >= 0:
        aligned[: samples.size - offset] = samples[offset:]
    else:
        aligned[-offset :] = samples[: samples.size + offset]
    aligned *= normalization_gain

    return {
        "arrival_sample": arrival,
        "threshold": float(threshold),
        "noise_sigma": float(noise_sigma),
        "direct_peak": peak,
        "offset_samples": offset,
        "normalization_gain": normalization_gain,
        "alignment_uncertainty_samples": 0.5,
        "aligned": aligned,
        "reference_arrival_sample": ref_arrival,
        "reference_threshold": float(ref_threshold),
        "reference_noise_sigma": float(ref_noise_sigma),
        "reference_direct_peak": ref_peak,
    }

