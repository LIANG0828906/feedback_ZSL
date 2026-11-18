"""Cue-driven sample alignment and early-reflection compensation."""

from __future__ import annotations

import math
import statistics
from typing import Any

import numpy as np

from .dsp import (
    estimate_onset,
    linear_to_db,
    peak,
    reflection_decay_envelope,
    rms,
    shift_samples,
)
from .errors import business_error


def _detect_cues(
    signal: np.ndarray,
    cues: list[float],
    sample_rate: int,
    search_before: int,
    search_after: int,
    prior_offset_samples: int = 0,
) -> tuple[list[int] | None, int]:
    onsets: list[int] = []
    centers: list[int] = []
    for cue in cues:
        reference_center = round(float(cue) * sample_rate)
        centers.append(reference_center)
        search_center = reference_center + int(prior_offset_samples)
        onset = estimate_onset(
            signal, search_center, search_before, search_after
        )
        if onset is None:
            return None, 0
        onsets.append(onset)
    if not onsets:
        return None, 0
    errors = [onset - center for onset, center in zip(onsets, centers)]
    return onsets, round(float(statistics.median(errors)))


def _aligned_deviations(
    signal: np.ndarray,
    cues: list[float],
    sample_rate: int,
) -> tuple[list[int], list[int]]:
    onsets: list[int] = []
    deviations: list[int] = []
    search = max(8, round(0.004 * sample_rate))
    for cue in cues:
        center = round(float(cue) * sample_rate)
        onset = estimate_onset(signal, center, search, search)
        if onset is None:
            onsets.append(-1)
            deviations.append(10**9)
            continue
        onsets.append(onset)
        deviations.append(onset - center)
    return onsets, deviations


def _window_bounds(
    reflection: dict[str, Any],
    sample_rate: int,
) -> tuple[int, int]:
    window = reflection["window_ms"]
    start = max(1, round(float(window[0]) * sample_rate / 1000.0))
    end = round(float(window[1]) * sample_rate / 1000.0)
    if end <= start:
        end = start + 1
    return start, end


def estimate_and_compensate_reflection(
    signal: np.ndarray,
    onsets: list[int],
    reflection: dict[str, Any],
    sample_rate: int,
) -> tuple[np.ndarray, dict[str, Any]]:
    """Estimate the single early echo, then remove it without touching direct."""

    enabled = bool(reflection.get("enabled", False))
    if not enabled:
        return signal.copy(), {
            "enabled": False,
            "delay_samples": None,
            "delay_ms": None,
            "estimated_gain_db": None,
            "declared_gain_db": None,
            "window_samples": None,
            "energy_before_db": None,
            "energy_after_db": None,
            "reduction_db": None,
            "direct_peak_before_dbfs": None,
            "direct_peak_after_dbfs": None,
            "direct_peak_loss_db": None,
            "reference_template_samples": None,
            "decay_span_samples": None,
            "estimation_status": "disabled",
        }

    start, end = _window_bounds(reflection, sample_rate)
    template_length = start
    envelope = reflection_decay_envelope(template_length, start, end)
    templates: list[np.ndarray] = []
    bases: list[np.ndarray] = []
    valid_onsets: list[int] = []
    for onset in onsets:
        if onset < 0 or onset + template_length > signal.size:
            continue
        template = np.asarray(
            signal[onset : onset + template_length], dtype=np.float64
        )
        if peak(template) <= 1.0e-9:
            continue
        templates.append(template)
        bases.append(template * envelope)
        valid_onsets.append(int(onset))

    if not templates:
        return signal.copy(), {
            "enabled": True,
            "delay_samples": None,
            "delay_ms": None,
            "estimated_gain_db": None,
            "declared_gain_db": float(reflection["gain_db"]),
            "window_samples": [start, end],
            "energy_before_db": None,
            "energy_after_db": None,
            "reduction_db": None,
            "direct_peak_before_dbfs": None,
            "direct_peak_after_dbfs": None,
            "direct_peak_loss_db": None,
            "reference_template_samples": template_length,
            "decay_span_samples": int(end - start),
            "estimation_status": "failed",
        }

    best_delay: int | None = None
    best_score = -math.inf
    for delay in range(start, end + 1):
        score = 0.0
        usable = True
        for onset, basis in zip(valid_onsets, bases):
            if onset + delay + basis.size > signal.size:
                usable = False
                break
            echo = signal[
                onset + delay : onset + delay + basis.size
            ]
            denominator = float(np.linalg.norm(basis) * np.linalg.norm(echo))
            if denominator <= 1.0e-12:
                usable = False
                break
            score += float(np.dot(basis, echo)) / denominator
        if usable and score > best_score:
            best_score = score
            best_delay = delay

    if best_delay is None or best_score < 0.20:
        return signal.copy(), {
            "enabled": True,
            "delay_samples": None,
            "delay_ms": None,
            "estimated_gain_db": None,
            "declared_gain_db": float(reflection["gain_db"]),
            "window_samples": [start, end],
            "energy_before_db": None,
            "energy_after_db": None,
            "reduction_db": None,
            "direct_peak_before_dbfs": None,
            "direct_peak_after_dbfs": None,
            "direct_peak_loss_db": None,
            "reference_template_samples": template_length,
            "decay_span_samples": int(end - start),
            "estimation_status": "failed",
        }

    numerator = 0.0
    denominator = 0.0
    for onset, basis in zip(valid_onsets, bases):
        echo = signal[
            onset + best_delay : onset + best_delay + basis.size
        ]
        numerator += float(np.dot(basis, echo))
        denominator += float(np.dot(basis, basis))
    estimated_gain = numerator / denominator if denominator > 0.0 else 0.0

    direct_peak_before = 0.0
    direct_peak_after = 0.0
    energy_before = 0.0
    corrected = signal.astype(np.float64, copy=True)

    for onset, basis in zip(valid_onsets, bases):
        direct_peak_before = max(
            direct_peak_before, peak(signal[onset : onset + start])
        )
        window_before = signal[onset + start : onset + end]
        energy_before += float(np.sum(window_before * window_before))
        subtract_start = onset + best_delay
        subtract_end = min(
            corrected.size, subtract_start + basis.size
        )
        if subtract_end > subtract_start:
            corrected[subtract_start:subtract_end] -= (
                estimated_gain * basis[: subtract_end - subtract_start]
            )

    for onset in valid_onsets:
        direct_peak_after = max(
            direct_peak_after, peak(corrected[onset : onset + start])
        )
    energy_after = 0.0
    for onset in valid_onsets:
        window_after = corrected[onset + start : onset + end]
        energy_after += float(np.sum(window_after * window_after))

    before_db = linear_to_db(math.sqrt(energy_before), -200.0)
    after_db = linear_to_db(math.sqrt(energy_after), -200.0)
    reduction = before_db - after_db
    direct_loss = linear_to_db(direct_peak_before) - linear_to_db(
        direct_peak_after
    )
    return corrected, {
        "enabled": True,
        "delay_samples": int(best_delay),
        "delay_ms": float(best_delay * 1000.0 / sample_rate),
        "estimated_gain_db": float(20.0 * math.log10(max(estimated_gain, 1.0e-12))),
        "declared_gain_db": float(reflection["gain_db"]),
        "window_samples": [int(start), int(end)],
        "energy_before_db": float(before_db),
        "energy_after_db": float(after_db),
        "reduction_db": float(reduction),
        "direct_peak_before_dbfs": float(linear_to_db(direct_peak_before)),
        "direct_peak_after_dbfs": float(linear_to_db(direct_peak_after)),
        "direct_peak_loss_db": float(direct_loss),
        "reference_template_samples": int(template_length),
        "decay_span_samples": int(end - start),
        "estimation_status": "estimated",
    }


def process_channels(
    loaded_channels: list[dict[str, Any]],
    document: dict[str, Any],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    sample_rate = int(document["sample_rate"])
    search = document.get("channel_search_ms", {})
    search_before = round(float(search.get("before", 120.0)) * sample_rate / 1000.0)
    search_after = round(float(search.get("after", 120.0)) * sample_rate / 1000.0)
    warnings: list[dict[str, Any]] = []
    processed: list[dict[str, Any]] = []

    for loaded in loaded_channels:
        channel = loaded["manifest"]
        name = loaded["name"]
        cues = [float(value) for value in channel["cues"]]
        fallback = int(channel["fallback_alignment_samples"])
        prior_physical_offset = (
            -fallback
            if fallback
            else int(channel.get("start_offset_samples", 0))
        )
        detected, correction = _detect_cues(
            loaded["data"],
            cues,
            sample_rate,
            search_before,
            search_after,
            prior_physical_offset,
        )
        if detected is None:
            alignment_status = "fallback"
            warnings.append(
                business_error(
                    "DETECTION_FAILED",
                    name,
                    "one or more cue transients could not be detected; "
                    "manifest fallback offset used",
                )
            )
            detected_before: list[int] | None = None
            alignment_offset = fallback
        else:
            alignment_status = "detected"
            detected_before = detected
            alignment_offset = -int(correction)

        aligned = shift_samples(loaded["data"], alignment_offset)
        post_onsets, deviations = _aligned_deviations(
            aligned, cues, sample_rate
        )
        finite_deviations = [
            value for value in deviations if abs(value) < 10**8
        ]
        max_deviation = (
            max(abs(value) for value in finite_deviations)
            if finite_deviations
            else None
        )
        if len(finite_deviations) != len(cues) or (
            max_deviation is not None and max_deviation > 1
        ):
            warnings.append(
                business_error(
                    "SYNC_TOLERANCE_EXCEEDED",
                    name,
                    "post-alignment onset deviation exceeds one sample",
                    max_abs_deviation_samples=max_deviation,
                )
            )

        reflection_config = channel.get("early_reflection", {"enabled": False})
        compensated, reflection_report = estimate_and_compensate_reflection(
            aligned, post_onsets, reflection_config, sample_rate
        )
        if reflection_report["estimation_status"] == "failed":
            warnings.append(
                business_error(
                    "REFLECTION_ESTIMATION_FAILED",
                    name,
                    "early reflection could not be estimated; channel left "
                    "uncompensated",
                )
            )
        elif reflection_report["enabled"]:
            if (
                reflection_report["reduction_db"] is not None
                and reflection_report["reduction_db"] < 12.0
            ):
                warnings.append(
                    business_error(
                        "REFLECTION_REDUCTION_INSUFFICIENT",
                        name,
                        "reflection-window energy reduction is below 12 dB",
                        reduction_db=reflection_report["reduction_db"],
                    )
                )
            if (
                reflection_report["direct_peak_loss_db"] is not None
                and reflection_report["direct_peak_loss_db"] > 1.0
            ):
                warnings.append(
                    business_error(
                        "DIRECT_PEAK_LOSS_EXCEEDED",
                        name,
                        "direct transient peak loss exceeds 1 dB",
                        direct_peak_loss_db=reflection_report[
                            "direct_peak_loss_db"
                        ],
                    )
                )

        weights = [float(value) for value in channel["mix_weights"]]
        weighted_energy = float(
            np.sum(compensated * compensated)
            * (weights[0] * weights[0] + weights[1] * weights[1])
        )
        processed.append(
            {
                "name": name,
                "instrument": loaded["instrument"],
                "file": loaded["file"],
                "declared_start_offset_samples": int(
                    channel["start_offset_samples"]
                ),
                "fallback_alignment_samples": fallback,
                "alignment_offset_samples": int(alignment_offset),
                "alignment_status": alignment_status,
                "detected_onsets_before_alignment": detected_before,
                "onsets_after_alignment": post_onsets,
                "post_alignment_deviations_samples": deviations,
                "max_abs_deviation_samples": max_deviation,
                "reflection": reflection_report,
                "mix_weights": weights,
                "weighted_energy": weighted_energy,
                "aligned_compensated": compensated,
            }
        )
    return processed, warnings


def mix_stereo(
    processed_channels: list[dict[str, Any]],
) -> tuple[np.ndarray, list[float], int]:
    total_frames = max(
        int(channel["aligned_compensated"].size)
        for channel in processed_channels
    )
    left = np.zeros(total_frames, dtype=np.float64)
    right = np.zeros(total_frames, dtype=np.float64)
    for channel in processed_channels:
        signal = channel["aligned_compensated"]
        left[: signal.size] += channel["mix_weights"][0] * signal
        right[: signal.size] += channel["mix_weights"][1] * signal
    total_energy = sum(
        float(channel["weighted_energy"]) for channel in processed_channels
    )
    contribution = [
        (
            float(channel["weighted_energy"]) / total_energy * 100.0
            if total_energy > 0.0
            else 0.0
        )
        for channel in processed_channels
    ]
    return np.column_stack([left, right]), contribution, total_frames


def rms_value(signal: np.ndarray) -> float:
    return rms(signal)

