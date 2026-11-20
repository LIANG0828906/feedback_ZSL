from __future__ import annotations

import math
from typing import Any

import numpy as np

from .bands import Band


def _angle_deg(value: float) -> float:
    return math.degrees(math.atan2(math.sin(value), math.cos(value)))


def _source_attribution(error_band: dict[str, Any]) -> tuple[str, dict[str, float]]:
    terms = error_band["terms_db"]
    candidates = {
        "alignment_offset": float(terms["alignment"]),
        "smoothing_coupling": float(terms["smoothing"]),
        "filter_truncation": float(terms["truncation"]),
    }
    source = max(candidates.items(), key=lambda item: (item[1], item[0]))[0]
    return source, candidates


def diagnose_consistency(
    channel_names: list[str],
    reference_channel: str,
    bands: tuple[Band, ...],
    analysis: dict[str, dict[str, object]],
    alignments: dict[str, dict[str, object]],
    errors: dict[str, list[dict[str, Any]]],
    filters: dict[str, dict[str, object]],
    gain_threshold_db: float,
    phase_threshold_deg: float,
    gain_jump_threshold_db: float,
    low_frequency_limit_hz: float,
) -> dict[str, object]:
    warnings: list[dict[str, object]] = []
    reference = reference_channel
    ref_db = np.asarray(analysis[reference]["power_db"], dtype=np.float64)
    ref_phase = np.asarray(analysis[reference]["phase_rad"], dtype=np.float64)

    for channel in channel_names:
        if channel == reference:
            continue
        channel_db = np.asarray(analysis[channel]["power_db"], dtype=np.float64)
        channel_phase = np.asarray(analysis[channel]["phase_rad"], dtype=np.float64)
        for index, band in enumerate(bands):
            gain_delta = float(channel_db[index] - ref_db[index])
            phase_delta = _angle_deg(float(channel_phase[index] - ref_phase[index]))
            if abs(gain_delta) > gain_threshold_db:
                warnings.append(
                    {
                        "kind": "gain_mismatch",
                        "channel": channel,
                        "reference_channel": reference,
                        "band_start": band.label,
                        "band_end": band.label,
                        "frequency_hz": band.center_hz,
                        "value_db": gain_delta,
                        "threshold_db": gain_threshold_db,
                        "source": "relative_channel_gain",
                        "evidence": {
                            "channel_power_db": float(channel_db[index]),
                            "reference_power_db": float(ref_db[index]),
                        },
                    }
                )
            if abs(phase_delta) > phase_threshold_deg:
                kind = "phase_inversion" if abs(phase_delta) > 150.0 else "phase_mismatch"
                warnings.append(
                    {
                        "kind": kind,
                        "channel": channel,
                        "reference_channel": reference,
                        "band_start": band.label,
                        "band_end": band.label,
                        "frequency_hz": band.center_hz,
                        "phase_delta_deg": phase_delta,
                        "threshold_deg": phase_threshold_deg,
                        "source": "channel_phase_difference",
                        "evidence": {
                            "channel_phase_deg": float(
                                analysis[channel]["phase_deg"][index]
                            ),
                            "reference_phase_deg": float(
                                analysis[reference]["phase_deg"][index]
                            ),
                        },
                    }
                )

    for channel in channel_names:
        channel_db = np.asarray(analysis[channel]["power_db"], dtype=np.float64)
        for index in range(1, len(bands)):
            jump_db = float(channel_db[index] - channel_db[index - 1])
            if abs(jump_db) <= gain_jump_threshold_db:
                continue
            band = bands[index]
            previous = bands[index - 1]
            error_band = errors[channel][index]
            source, candidates = _source_attribution(error_band)
            intermediates = error_band["intermediates"]
            warnings.append(
                {
                    "kind": (
                        "low_frequency_gain_jump"
                        if band.center_hz <= low_frequency_limit_hz
                        else "gain_jump"
                    ),
                    "channel": channel,
                    "band_start": previous.label,
                    "band_end": band.label,
                    "frequency_hz": band.center_hz,
                    "jump_db": jump_db,
                    "threshold_db": gain_jump_threshold_db,
                    "source": source if band.center_hz <= low_frequency_limit_hz else "band_delta",
                    "attribution": candidates,
                    "evidence": {
                        "offset_samples": float(
                            alignments[channel]["offset_samples"]
                        ),
                        "smoothing_coupling_power": float(
                            intermediates["smoothing_coupling_bound"]
                        ),
                        "smoothing_max_neighbor_weight": float(
                            intermediates["max_neighbor_weight"]
                        ),
                        "truncation_db": float(intermediates["truncation_db"]),
                        "error_total_db": float(error_band["total_error_db"]),
                        "filter_group_delay_samples": float(
                            filters[channel]["group_delay_samples"]
                        ),
                    },
                }
            )

    warnings.sort(
        key=lambda item: (
            str(item["channel"]),
            str(item["kind"]),
            float(item.get("frequency_hz", 0.0)),
        )
    )
    return {
        "reference_channel": reference,
        "warnings": warnings,
        "warning_count": len(warnings),
        "channels": channel_names,
        "thresholds": {
            "gain_db": float(gain_threshold_db),
            "phase_deg": float(phase_threshold_deg),
            "gain_jump_db": float(gain_jump_threshold_db),
            "low_frequency_limit_hz": float(low_frequency_limit_hz),
        },
    }

