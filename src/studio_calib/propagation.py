from __future__ import annotations

import math

import numpy as np


def _db_from_power_ratio(bound: float, power: float) -> float:
    safe_power = max(float(power), np.finfo(np.float64).tiny)
    ratio = max(float(bound), 0.0) / safe_power
    return 10.0 * math.log10(1.0 + ratio)


def alignment_power_fraction(
    frequency_hz: float, sample_rate: int, uncertainty_samples: float
) -> float:
    phase = math.pi * frequency_hz * uncertainty_samples / sample_rate
    return float((2.0 * math.sin(phase)) ** 2)


def build_error_band(
    channel: str,
    band_label: str,
    frequency_hz: float,
    sample_rate: int,
    alignment_uncertainty_samples: float,
    smoothed_power: float,
    propagated_quantization_bound: float,
    propagated_segmentation_bound: float,
    smoothing_coupling_bound: float,
    truncation_db: float,
    threshold_db: float,
    intermediates: dict[str, object],
) -> dict[str, object]:
    alignment_fraction = alignment_power_fraction(
        frequency_hz, sample_rate, alignment_uncertainty_samples
    )
    quantization_fraction = max(
        propagated_quantization_bound, 0.0
    ) / max(smoothed_power, np.finfo(np.float64).tiny)
    segmentation_fraction = max(
        propagated_segmentation_bound, 0.0
    ) / max(smoothed_power, np.finfo(np.float64).tiny)
    smoothing_fraction = max(smoothing_coupling_bound, 0.0) / max(
        smoothed_power, np.finfo(np.float64).tiny
    )
    truncation_fraction = float(10.0 ** (max(truncation_db, 0.0) / 10.0) - 1.0)

    fractions = {
        "alignment": alignment_fraction,
        "quantization": quantization_fraction,
        "segmentation": segmentation_fraction,
        "smoothing": smoothing_fraction,
        "truncation": truncation_fraction,
    }
    terms_db = {
        name: 10.0 * math.log10(1.0 + max(value, 0.0))
        for name, value in fractions.items()
    }
    total_db = float(sum(terms_db.values()))
    dominant_node = max(terms_db.items(), key=lambda item: (item[1], item[0]))[0]
    status = "E_ERROR_BUDGET" if total_db > threshold_db else "OK"
    return {
        "channel": channel,
        "band": band_label,
        "frequency_hz": float(frequency_hz),
        "terms_db": terms_db,
        "terms_power_fraction": fractions,
        "total_error_db": total_db,
        "threshold_db": float(threshold_db),
        "status": status,
        "dominant_node": dominant_node,
        "recomputation": {
            "formula": "sum(10*log10(1+fraction))",
            "fractions": fractions,
        },
        "intermediates": intermediates,
    }

